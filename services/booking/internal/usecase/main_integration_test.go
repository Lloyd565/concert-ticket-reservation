//go:build integration

// Integration tests for the Booking service's use cases.
//
// These run against a real PostgreSQL container, never a mock. A mock cannot
// exhibit a race condition, so a mocked version of the concurrency test below
// would prove nothing at all (AGENTS.md §7).
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
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/repository/postgres"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/usecase"
)

// testPool is shared by every test in this package: one container per package
// run, not one per test.
var testPool *pgxpool.Pool

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

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		os.Exit(1)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse dsn: %v\n", err)
		os.Exit(1)
	}
	// Enough connections that the concurrency test contends on the seat row
	// rather than on the pool - the pool queue would serialise callers before
	// they ever reached SELECT ... FOR UPDATE and the test would pass for the
	// wrong reason.
	cfg.MaxConns = 32

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
	os.Exit(code)
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

// fixture is the wiring one test needs. Each test seeds its own event, so tests
// never contend with each other over the same seats.
//
// Postgres is real in every test here. Payment is not: it is the fake below,
// because the failure the saga most needs to survive - a payment call that
// never answers - is one no real service will produce on demand. That is the
// division AGENTS.md §7 draws. Never mock the database in a test that validates
// locking; do inject faults into the dependency whose faults are the subject.
type fixture struct {
	repo       *postgres.Repo
	holder     *usecase.Holder
	seeder     *usecase.Seeder
	sweeper    *usecase.Sweeper
	saga       *usecase.Saga
	reconciler *usecase.Reconciler
	payment    *fakePayment
}

func newFixture(t *testing.T, holdTTL time.Duration) *fixture {
	t.Helper()
	repo := postgres.NewRepo(testPool)
	txm := postgres.NewTxManager(testPool)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	payment := newFakePayment()
	saga := usecase.NewSaga(txm, repo, repo, repo, payment, log)
	return &fixture{
		repo:    repo,
		holder:  usecase.NewHolder(txm, repo, repo, holdTTL),
		seeder:  usecase.NewSeeder(txm, repo),
		sweeper: usecase.NewSweeper(repo, time.Second, log),
		saga:    saga,
		// A long grace period, which is also what isolates these tests from
		// each other: the reconciliation scan is global, so a test opts its own
		// reservation in by backdating payment_pending_since (see
		// awaitsReconciliation) rather than by racing a short timer.
		reconciler: usecase.NewReconciler(saga, repo, payment, time.Second, time.Hour, 0, log),
		payment:    payment,
	}
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
