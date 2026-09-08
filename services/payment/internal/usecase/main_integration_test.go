//go:build integration

// Integration tests for the Payment service's use cases.
//
// These run against a real PostgreSQL container. The idempotency guarantees
// under test are enforced by a UNIQUE index and by WHERE clauses, and a mocked
// repository would simply agree with whatever the code believed - which is the
// same reason the Booking concurrency test refuses a mock (AGENTS.md §7).
//
// The payment provider IS faked, because the failure that matters most here -
// a provider that never answers - is one no real provider produces on demand.
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

	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/provider"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/repository/postgres"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/usecase"
)

// testPool is shared by every test in this package: one container per package
// run, not one per test.
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("payment_db"),
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
	testPool, err = pgxpool.New(ctx, dsn)
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

// fixture is the wiring one test needs.
type fixture struct {
	repo     *postgres.Repo
	provider *provider.Mock
	charger  *usecase.Charger
	refunder *usecase.Refunder
}

// newFixture wires a Charger and Refunder over a real database and the mock
// provider in the given mode. The provider timeout is short so a hanging
// provider fails the test fast rather than slowly.
func newFixture(t *testing.T, mode provider.Mode) *fixture {
	t.Helper()
	repo := postgres.NewRepo(testPool)
	prov := provider.NewMock(mode, 0)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return &fixture{
		repo:     repo,
		provider: prov,
		charger:  usecase.NewCharger(repo, prov, 100*time.Millisecond, 1, log),
		refunder: usecase.NewRefunder(repo, repo, prov, 100*time.Millisecond, log),
	}
}

// chargeRow reads a charge straight from the database, bypassing every layer
// under test.
func chargeRow(t *testing.T, ctx context.Context, key string) (status string, providerRef *string) {
	t.Helper()
	if err := testPool.QueryRow(ctx,
		`SELECT status, provider_ref FROM charges WHERE idempotency_key = $1`, key).Scan(&status, &providerRef); err != nil {
		t.Fatalf("read charge: %v", err)
	}
	return status, providerRef
}

func countCharges(t *testing.T, ctx context.Context, reservationID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM charges WHERE reservation_id = $1`, reservationID).Scan(&n); err != nil {
		t.Fatalf("count charges: %v", err)
	}
	return n
}
