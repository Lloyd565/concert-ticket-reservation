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
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

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
type fixture struct {
	repo    *postgres.Repo
	holder  *usecase.Holder
	seeder  *usecase.Seeder
	sweeper *usecase.Sweeper
}

func newFixture(t *testing.T, holdTTL time.Duration) *fixture {
	t.Helper()
	repo := postgres.NewRepo(testPool)
	txm := postgres.NewTxManager(testPool)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return &fixture{
		repo:    repo,
		holder:  usecase.NewHolder(txm, repo, repo, holdTTL),
		seeder:  usecase.NewSeeder(txm, repo),
		sweeper: usecase.NewSweeper(repo, time.Second, log),
	}
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
