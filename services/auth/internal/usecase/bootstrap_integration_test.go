//go:build integration

package usecase_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/usecase"
)

// withoutAdmins establishes the "no admin yet" precondition for real, rather
// than assuming it from test ordering, and puts the database back afterwards.
//
// The bootstrap's guard is global to the database, so these tests must not run
// beside anything else that creates an admin. Nothing in this package calls
// t.Parallel, so sequential execution is the whole of the coordination.
func withoutAdmins(t *testing.T) {
	t.Helper()
	clear := func() {
		if _, err := testPool.Exec(context.Background(), `DELETE FROM users WHERE role = 'admin'`); err != nil {
			t.Fatalf("clear admins: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)
}

func adminCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE role = 'admin'`).Scan(&n); err != nil {
		t.Fatalf("count admins: %v", err)
	}
	return n
}

// TestBootstrapAdminCreatesThenNoOps is the whole contract: it creates the first
// admin, and every later call changes nothing. Startup runs it unconditionally,
// so "safe to repeat" is not a nicety.
func TestBootstrapAdminCreatesThenNoOps(t *testing.T) {
	withoutAdmins(t)
	ctx := context.Background()
	accounts := newAccounts(t, 15*time.Minute)
	email := uniqueEmail(t)
	const password = "bootstrap-admin-password"

	outcome, err := accounts.BootstrapAdmin(ctx, email, password)
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	if outcome != usecase.BootstrapCreated {
		t.Fatalf("first bootstrap outcome = %v, want BootstrapCreated", outcome)
	}
	if n := adminCount(t); n != 1 {
		t.Fatalf("admin count = %d, want 1", n)
	}

	// The account is real: it can log in and its token carries the admin role,
	// which is what the gateway authorizes privileged registration on.
	session, err := accounts.Login(ctx, email, password)
	if err != nil {
		t.Fatalf("login as the bootstrapped admin: %v", err)
	}
	if session.Role != domain.RoleAdmin {
		t.Errorf("bootstrapped role = %q, want admin", session.Role)
	}
	assertVerifiableToken(t, session.AccessToken, session.UserID, string(domain.RoleAdmin))

	// Restart, twice. Neither call may create a second account.
	for i := 2; i <= 3; i++ {
		outcome, err := accounts.BootstrapAdmin(ctx, email, password)
		if err != nil {
			t.Fatalf("bootstrap call %d: %v", i, err)
		}
		if outcome != usecase.BootstrapAdminPresent {
			t.Errorf("bootstrap call %d outcome = %v, want BootstrapAdminPresent", i, outcome)
		}
		if n := adminCount(t); n != 1 {
			t.Fatalf("admin count after call %d = %d, want 1", i, n)
		}
	}
}

// TestBootstrapAdminNoOpsForADifferentExistingAdmin: the guard is the role, not
// the configured address. Changing BOOTSTRAP_ADMIN_EMAIL later must not mint a
// second admin behind the operator's back.
func TestBootstrapAdminNoOpsForADifferentExistingAdmin(t *testing.T) {
	withoutAdmins(t)
	ctx := context.Background()
	accounts := newAccounts(t, 15*time.Minute)

	if _, err := accounts.BootstrapAdmin(ctx, uniqueEmail(t), "bootstrap-admin-password"); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	outcome, err := accounts.BootstrapAdmin(ctx, uniqueEmail(t), "a-different-admin-password")
	if err != nil {
		t.Fatalf("second bootstrap with a new address: %v", err)
	}
	if outcome != usecase.BootstrapAdminPresent {
		t.Errorf("outcome = %v, want BootstrapAdminPresent", outcome)
	}
	if n := adminCount(t); n != 1 {
		t.Errorf("admin count = %d, want 1 - a changed address minted a second admin", n)
	}
}

// TestBootstrapAdminConcurrent is the multi-replica case: several instances
// start together, all see no admin, and all try to insert. Exactly one may
// succeed, and none may fail in a way that would kill its own startup.
func TestBootstrapAdminConcurrent(t *testing.T) {
	withoutAdmins(t)
	ctx := context.Background()
	accounts := newAccounts(t, 15*time.Minute)
	email := uniqueEmail(t)
	const password = "bootstrap-admin-password"

	const replicas = 8
	outcomes := make([]usecase.BootstrapOutcome, replicas)
	errs := make([]error, replicas)

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := range replicas {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait() // release them together
			outcomes[i], errs[i] = accounts.BootstrapAdmin(ctx, email, password)
		}()
	}
	start.Done()
	done.Wait()

	created := 0
	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d failed its bootstrap: %v", i, err)
		}
		if outcomes[i] == usecase.BootstrapCreated {
			created++
		}
	}
	if created != 1 {
		t.Errorf("%d replicas reported creating the admin, want exactly 1", created)
	}
	if n := adminCount(t); n != 1 {
		t.Errorf("admin count = %d, want 1", n)
	}
}

// TestBootstrapAdminLeavesAnExistingAccountAlone: an address already registered
// as an attendee must not be silently promoted. Reading a privilege change out
// of an environment variable is a larger blast radius than this feature is
// worth.
func TestBootstrapAdminLeavesAnExistingAccountAlone(t *testing.T) {
	withoutAdmins(t)
	ctx := context.Background()
	accounts := newAccounts(t, 15*time.Minute)
	email := uniqueEmail(t)

	existing, err := accounts.Register(ctx, email, "an-ordinary-attendee-password", "")
	if err != nil {
		t.Fatalf("register the attendee: %v", err)
	}

	outcome, err := accounts.BootstrapAdmin(ctx, email, "bootstrap-admin-password")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if outcome != usecase.BootstrapEmailTaken {
		t.Errorf("outcome = %v, want BootstrapEmailTaken", outcome)
	}
	if n := adminCount(t); n != 0 {
		t.Errorf("admin count = %d, want 0", n)
	}

	var role string
	if err := testPool.QueryRow(ctx, `SELECT role FROM users WHERE id = $1`, existing.ID).Scan(&role); err != nil {
		t.Fatalf("re-read the attendee: %v", err)
	}
	if role != string(domain.RoleAttendee) {
		t.Errorf("role = %q, want it left as attendee", role)
	}
	// The original password still works, so the bootstrap did not overwrite it.
	if _, err := accounts.Login(ctx, email, "an-ordinary-attendee-password"); err != nil {
		t.Errorf("the existing account's password stopped working: %v", err)
	}
}

// TestBootstrapAdminRejectsAWeakPassword: the bootstrap account gets the same
// rules as any other, and the error is one runBootstrap treats as permanent.
func TestBootstrapAdminRejectsAWeakPassword(t *testing.T) {
	withoutAdmins(t)
	ctx := context.Background()
	accounts := newAccounts(t, 15*time.Minute)

	_, err := accounts.BootstrapAdmin(ctx, uniqueEmail(t), "short")
	if err == nil {
		t.Fatal("a five-character bootstrap password was accepted")
	}
	if !errors.Is(err, domain.ErrWeakPassword) {
		t.Errorf("error = %v, want it to wrap ErrWeakPassword", err)
	}
	if n := adminCount(t); n != 0 {
		t.Errorf("admin count = %d, want 0", n)
	}
}
