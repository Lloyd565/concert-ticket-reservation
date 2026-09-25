//go:build integration

// Integration tests for the Auth service, against a real PostgreSQL container.
//
// The repository is not mocked here (AGENTS.md §7): the properties under test -
// the email UNIQUE index, and revocation being a conditional UPDATE that can
// only fire once - are properties of the schema, and a fake would simply
// re-implement them and agree with itself.
package usecase_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/repository/postgres"
	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/token"
	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/usecase"
)

const (
	testSecret   = "integration-secret-at-least-32-bytes!"
	testIssuer   = "concert-auth"
	testAudience = "concert-api"
)

// testPool is shared by every test in this package: one container per package
// run, not one per test.
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("auth_db"),
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

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		_ = testcontainers.TerminateContainer(container)
		os.Exit(1)
	}

	testPool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect pool: %v\n", err)
		_ = testcontainers.TerminateContainer(container)
		os.Exit(1)
	}
	if err := applyMigrations(ctx, testPool); err != nil {
		fmt.Fprintf(os.Stderr, "apply migrations: %v\n", err)
		_ = testcontainers.TerminateContainer(container)
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

// newAccounts wires the real repository against the test database. accessTTL is
// a parameter because one test needs a token that is already expired.
func newAccounts(t *testing.T, accessTTL time.Duration) *usecase.Accounts {
	t.Helper()
	repo := postgres.NewRepo(testPool)
	signer := token.NewSigner([]byte(testSecret), testIssuer, testAudience, accessTTL)
	return usecase.NewAccounts(repo, repo, signer, 7*24*time.Hour)
}

// uniqueEmail keeps tests from colliding in the shared database.
func uniqueEmail(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-%d@example.com", t.Name(), time.Now().UnixNano())
}
