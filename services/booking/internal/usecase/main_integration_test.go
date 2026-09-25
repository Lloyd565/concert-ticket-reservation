//go:build integration

// Integration tests for the Booking service's use cases.
//
// These run against a real PostgreSQL container and a real Redis container,
// never a mock. A mock cannot exhibit a race condition, so a mocked version of
// the concurrency test would prove nothing at all (AGENTS.md §7). In P3 that
// applies to Redis first of all: Redis is the lock now, and a fake one would be
// a fake proof.
package usecase_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/repository/postgres"
	bookingredis "github.com/lloyd565/concert-ticket-reservation/services/booking/internal/repository/redis"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/usecase"
)

// The shared infrastructure: one Postgres container and one Redis container per
// package run, not one per test. testPool is for assertions only - the code
// under test reaches the database through per-replica pools (see newReplicas).
var (
	testPool      *pgxpool.Pool
	testDSN       string
	testRedisAddr string
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("booking_db"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres container: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = testcontainers.TerminateContainer(container) }()

	testDSN, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		os.Exit(1)
	}

	redisContainer, redisAddr, err := startRedis(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start redis container: %v\n", err)
		os.Exit(1)
	}
	testRedisAddr = redisAddr
	defer func() { _ = testcontainers.TerminateContainer(redisContainer) }()

	cfg, err := pgxpool.ParseConfig(testDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse dsn: %v\n", err)
		os.Exit(1)
	}
	// Small on purpose: this pool only runs the SQL that tests assert with. The
	// connections that matter belong to the replicas.
	cfg.MaxConns = 8

	testPool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect pool: %v\n", err)
		os.Exit(1)
	}
	if err := applyMigrations(ctx, testPool); err != nil {
		fmt.Fprintf(os.Stderr, "apply migrations: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	testPool.Close()
	_ = testcontainers.TerminateContainer(container)
	_ = testcontainers.TerminateContainer(redisContainer)
	os.Exit(code)
}

// startRedis brings up a Redis container and returns it with its host address.
//
// Returned rather than hidden, because one test needs to kill it: proving that
// losing Redis fails holds closed (D12) means actually losing Redis, not
// pointing a client at a port nobody is listening on.
func startRedis(ctx context.Context) (testcontainers.Container, string, error) {
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return nil, "", err
	}
	host, err := c.Host(ctx)
	if err != nil {
		return c, "", err
	}
	port, err := c.MappedPort(ctx, "6379/tcp")
	if err != nil {
		return c, "", err
	}
	return c, fmt.Sprintf("%s:%s", host, port.Port()), nil
}

// applyMigrations runs the checked-in .up.sql files against the test database.
// The schema under test is the schema that ships - not a hand-written copy that
// can drift away from it.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.up.sql"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no migrations found")
	}
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
	}
	return nil
}

// fixture is one Booking replica's worth of wiring.
//
// "Replica" is meant literally. Each one gets its own connection pool and its
// own Redis client, and two of them share nothing in this process: no mutex, no
// map, no channel. Everything they agree about, they agree about through
// Postgres and Redis - which is exactly the relationship two containers have.
// That is what makes the multi-replica concurrency test worth running.
//
// Postgres and Redis are real in every test here. Payment is not: it is the
// fake below, because the failure the saga most needs to survive - a payment
// call that never answers - is one no real service will produce on demand. That
// is the division AGENTS.md §7 draws. Never mock the store whose concurrency is
// the subject; do inject faults into the dependency whose faults are.
type fixture struct {
	repo       *postgres.Repo
	tx         *postgres.TxManager
	holds      *bookingredis.Holds
	redis      *goredis.Client
	holder     *usecase.Holder
	seeder     *usecase.Seeder
	saga       *usecase.Saga
	reconciler *usecase.Reconciler
	expirer    *usecase.Expirer
	payment    *fakePayment
}

func newFixture(t *testing.T, holdTTL time.Duration) *fixture {
	t.Helper()
	return newReplicas(t, 1, holdTTL)[0]
}

// newReplicas wires n independent replicas against the shared Postgres and
// Redis.
func newReplicas(t *testing.T, n int, holdTTL time.Duration) []*fixture {
	t.Helper()
	out := make([]*fixture, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, newReplicaAt(t, holdTTL, testRedisAddr))
	}
	return out
}

// newReplicaAt wires one replica against a named Redis, so a test can stand a
// replica up in front of a Redis it is about to kill.
func newReplicaAt(t *testing.T, holdTTL time.Duration, redisAddr string) *fixture {
	t.Helper()

	cfg, err := pgxpool.ParseConfig(testDSN)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	// Enough connections that the concurrency test contends on seat state
	// rather than on the pool - a pool queue would serialise callers before
	// they reached the claim and the test would pass for the wrong reason.
	cfg.MaxConns = 24
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect replica pool: %v", err)
	}
	t.Cleanup(pool.Close)

	rdb := goredis.NewClient(&goredis.Options{Addr: redisAddr})
	t.Cleanup(func() { _ = rdb.Close() })
	holds := bookingredis.NewHolds(rdb)

	repo := postgres.NewRepo(pool)
	txm := postgres.NewTxManager(pool)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	payment := newFakePayment()
	saga := usecase.NewSaga(txm, holds, repo, repo, repo, repo, payment, holdTTL, log)

	return &fixture{
		repo:    repo,
		tx:      txm,
		holds:   holds,
		redis:   rdb,
		holder:  usecase.NewHolder(txm, holds, repo, repo, repo, holdTTL, log),
		expirer: usecase.NewExpirer(txm, repo, repo, log),
		seeder:  usecase.NewSeeder(txm, repo),
		saga:    saga,
		// A long grace period, which is also what isolates these tests from
		// each other: the reconciliation scan is global, so a test opts its own
		// reservation in by backdating payment_pending_since (see
		// awaitsReconciliation) rather than by racing a short timer.
		reconciler: usecase.NewReconciler(saga, repo, payment, time.Second, time.Hour, 0, log),
		payment:    payment,
	}
}

// holdKey is the key layout under test, spelled out here rather than imported
// so that a change to it has to be made deliberately in two places.
func holdKey(eventID, seatID string) string { return "seat:hold:" + eventID + ":" + seatID }

// holder returns the reservation currently holding a seat, or "" if none is.
func holder(t *testing.T, ctx context.Context, f *fixture, eventID, seatID string) string {
	t.Helper()
	v, err := f.redis.Get(ctx, holdKey(eventID, seatID)).Result()
	if err == goredis.Nil {
		return ""
	}
	if err != nil {
		t.Fatalf("read hold key: %v", err)
	}
	return v
}

// paymentMode is how the fake Payment service answers.
type paymentMode string

const (
	paymentSucceeds paymentMode = "succeeds"
	paymentDeclines paymentMode = "declines"
	// paymentTimesOut is the case D8 exists for: the call does not complete and
	// the caller learns nothing. What Payment is left holding is controlled
	// separately by onTimeout, because each variant is real and the saga has to
	// be correct for all of them.
	paymentTimesOut paymentMode = "times out"
)

// fakePayment stands in for the Payment service at the usecase port.
type fakePayment struct {
	mu   sync.Mutex
	mode paymentMode
	// onTimeout is what Payment is left holding when a call times out, and its
	// three values are three genuinely different situations Booking cannot tell
	// apart from the outside:
	//
	//	""               the call never arrived; no charge exists
	//	PaymentSucceeded the money moved and the answer was lost coming back
	//	PaymentPending   Payment recorded the charge, then its own provider call
	//	                 was interrupted, so the row is stuck unsettled
	//
	// Being unable to distinguish them is the entire reason Booking may not
	// release seats on a timeout.
	onTimeout domain.PaymentStatus
	// hangFor makes a timing-out call block, so the caller's own deadline is
	// what ends it - which is how a real timeout happens, and the only way to
	// reproduce a request context that dies mid-call.
	hangFor     time.Duration
	charges     map[string]domain.PaymentResult
	refunds     map[string]int64
	chargeCalls int
	// onCharge, when set, runs at the start of every Charge call: a view of what
	// Booking has already committed while its call is in flight, which is all a
	// Booking that crashes mid-call leaves behind.
	onCharge func()
}

func newFakePayment() *fakePayment {
	return &fakePayment{
		mode:    paymentSucceeds,
		charges: make(map[string]domain.PaymentResult),
		refunds: make(map[string]int64),
	}
}

func (f *fakePayment) setMode(mode paymentMode, onTimeout domain.PaymentStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode, f.onTimeout = mode, onTimeout
}

func (f *fakePayment) setHangFor(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hangFor = d
}

func (f *fakePayment) Charge(ctx context.Context, req usecase.ChargeRequest) (domain.PaymentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chargeCalls++
	if f.onCharge != nil {
		f.onCharge()
	}

	// Idempotent by key, exactly like the real service: a replay returns the
	// original settled answer rather than charging again.
	if prior, ok := f.charges[req.IdempotencyKey]; ok && prior.Status.Settled() {
		return prior, nil
	}

	switch f.mode {
	case paymentDeclines:
		res := domain.PaymentResult{
			ChargeID:      uuid.Must(uuid.NewV7()).String(),
			Status:        domain.PaymentDeclined,
			AmountCents:   req.AmountCents,
			DeclineReason: "card declined by issuer",
		}
		f.charges[req.IdempotencyKey] = res
		return res, nil
	case paymentTimesOut:
		if f.onTimeout != "" {
			f.charges[req.IdempotencyKey] = domain.PaymentResult{
				ChargeID:    uuid.Must(uuid.NewV7()).String(),
				Status:      f.onTimeout,
				AmountCents: req.AmountCents,
			}
		}
		// Wait out the caller's deadline when asked to. hangFor is zero by
		// default, so tests that only care about the outcome get it at once.
		select {
		case <-ctx.Done():
		case <-time.After(f.hangFor):
		}
		return domain.PaymentResult{}, fmt.Errorf("simulated timeout: %w", domain.ErrPaymentOutcomeUnknown)
	default:
		res := domain.PaymentResult{
			ChargeID:    uuid.Must(uuid.NewV7()).String(),
			Status:      domain.PaymentSucceeded,
			AmountCents: req.AmountCents,
		}
		f.charges[req.IdempotencyKey] = res
		return res, nil
	}
}

func (f *fakePayment) GetCharge(_ context.Context, idempotencyKey string) (domain.PaymentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mode == paymentTimesOut {
		// Payment is unreachable, so reconciliation learns nothing and must
		// change nothing.
		return domain.PaymentResult{}, fmt.Errorf("simulated timeout: %w", domain.ErrPaymentOutcomeUnknown)
	}
	res, ok := f.charges[idempotencyKey]
	if !ok {
		// Payment has never seen this key: no charge was ever created.
		return domain.PaymentResult{Status: domain.PaymentNoCharge}, nil
	}
	return res, nil
}

func (f *fakePayment) Refund(_ context.Context, chargeID string, amountCents int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refunds[chargeID] = amountCents
	return nil
}

func (f *fakePayment) refundedAmount(chargeID string) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	amount, ok := f.refunds[chargeID]
	return amount, ok
}

// reachable makes the fake answer GetCharge again after a timeout, so a test
// can watch reconciliation converge once the dependency comes back.
func (f *fakePayment) reachable() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = paymentSucceeds
}

// seedOneSeat creates an event whose seat map is a single seat - the sharpest
// possible version of the contention this system exists to survive.
func seedOneSeat(t *testing.T, f *fixture) (eventID, seatID string) {
	t.Helper()
	ev, seats, err := f.seeder.Seed(context.Background(), usecase.SeedSpec{
		Name:        t.Name(),
		Sections:    []string{"A"},
		Rows:        1,
		SeatsPerRow: 1,
	})
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return ev.ID, seats[0].ID
}

// seedSpecFor builds a one-row seat map with n seats.
func seedSpecFor(t *testing.T, n int) usecase.SeedSpec {
	t.Helper()
	return usecase.SeedSpec{Name: t.Name(), Sections: []string{"A"}, Rows: 1, SeatsPerRow: n}
}
