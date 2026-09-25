//go:build integration

package usecase_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/domain"
)

func TestRegisterAndLogin(t *testing.T) {
	ctx := context.Background()
	accounts := newAccounts(t, 15*time.Minute)
	email := uniqueEmail(t)
	const password = "correct-horse-battery-staple"

	user, err := accounts.Register(ctx, email, password, "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// An omitted role means the least privilege, never an accidental upgrade.
	if user.Role != domain.RoleAttendee {
		t.Errorf("role = %q, want attendee", user.Role)
	}
	// The address is normalised on the way in, so the UNIQUE index actually
	// prevents a duplicate account under a different capitalisation.
	if user.Email != strings.ToLower(email) {
		t.Errorf("email = %q, want it normalised", user.Email)
	}

	session, err := accounts.Login(ctx, strings.ToUpper(email), password)
	if err != nil {
		t.Fatalf("login with a differently-cased address: %v", err)
	}
	if session.UserID != user.ID {
		t.Errorf("session user = %q, want %q", session.UserID, user.ID)
	}
	assertVerifiableToken(t, session.AccessToken, user.ID, string(domain.RoleAttendee))
}

// TestDuplicateRegistrationRejected exercises the email UNIQUE index, which is
// the only thing that actually prevents two accounts for one address - the
// application check would race with itself.
func TestDuplicateRegistrationRejected(t *testing.T) {
	ctx := context.Background()
	accounts := newAccounts(t, 15*time.Minute)
	email := uniqueEmail(t)

	if _, err := accounts.Register(ctx, email, "correct-horse-battery-staple", ""); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// Different case, so this only fails if normalisation and the index agree.
	_, err := accounts.Register(ctx, strings.ToUpper(email), "another-long-password", "")
	if !errors.Is(err, domain.ErrEmailTaken) {
		t.Fatalf("second register error = %v, want ErrEmailTaken", err)
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	ctx := context.Background()
	accounts := newAccounts(t, 15*time.Minute)
	email := uniqueEmail(t)

	if _, err := accounts.Register(ctx, email, "correct-horse-battery-staple", "organizer"); err != nil {
		t.Fatalf("register: %v", err)
	}

	t.Run("wrong password", func(t *testing.T) {
		_, err := accounts.Login(ctx, email, "not-the-password")
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("error = %v, want ErrInvalidCredentials", err)
		}
	})
	t.Run("unknown account", func(t *testing.T) {
		// Same sentinel as above, deliberately: distinguishing them would turn
		// login into an account-enumeration oracle.
		_, err := accounts.Login(ctx, uniqueEmail(t), "correct-horse-battery-staple")
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("error = %v, want ErrInvalidCredentials", err)
		}
	})
}

func TestRegisterRejectsWeakInput(t *testing.T) {
	ctx := context.Background()
	accounts := newAccounts(t, 15*time.Minute)

	if _, err := accounts.Register(ctx, uniqueEmail(t), "short", ""); !errors.Is(err, domain.ErrWeakPassword) {
		t.Errorf("short password error = %v, want ErrWeakPassword", err)
	}
	if _, err := accounts.Register(ctx, "not-an-email", "correct-horse-battery-staple", ""); !errors.Is(err, domain.ErrInvalidInput) {
		t.Errorf("bad email error = %v, want ErrInvalidInput", err)
	}
	if _, err := accounts.Register(ctx, uniqueEmail(t), "correct-horse-battery-staple", "superuser"); !errors.Is(err, domain.ErrInvalidRole) {
		t.Errorf("bad role error = %v, want ErrInvalidRole", err)
	}
}

// TestRefreshRotates: a refresh must yield a new pair and kill the old token.
func TestRefreshRotates(t *testing.T) {
	ctx := context.Background()
	accounts := newAccounts(t, 15*time.Minute)
	email := uniqueEmail(t)
	const password = "correct-horse-battery-staple"

	if _, err := accounts.Register(ctx, email, password, "organizer"); err != nil {
		t.Fatalf("register: %v", err)
	}
	first, err := accounts.Login(ctx, email, password)
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	second, err := accounts.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh returned the same refresh token - it did not rotate")
	}
	if second.UserID != first.UserID {
		t.Errorf("refreshed session user = %q, want %q", second.UserID, first.UserID)
	}
	// The role survives the rotation, because it is read from the user row.
	assertVerifiableToken(t, second.AccessToken, first.UserID, string(domain.RoleOrganizer))
}

// TestReplayedRefreshTokenKillsTheChain: presenting an already-rotated token is
// evidence the chain leaked, so every live session for that user is cut.
func TestReplayedRefreshTokenKillsTheChain(t *testing.T) {
	ctx := context.Background()
	accounts := newAccounts(t, 15*time.Minute)
	email := uniqueEmail(t)
	const password = "correct-horse-battery-staple"

	if _, err := accounts.Register(ctx, email, password, ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	first, err := accounts.Login(ctx, email, password)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	// A second, independent session for the same user - the one that gets cut.
	other, err := accounts.Login(ctx, email, password)
	if err != nil {
		t.Fatalf("second login: %v", err)
	}

	second, err := accounts.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Replay the token that was just rotated away.
	if _, err := accounts.Refresh(ctx, first.RefreshToken); !errors.Is(err, domain.ErrInvalidRefreshToken) {
		t.Fatalf("replayed refresh error = %v, want ErrInvalidRefreshToken", err)
	}
	// Both the rotated-to token and the unrelated session are now dead.
	if _, err := accounts.Refresh(ctx, second.RefreshToken); !errors.Is(err, domain.ErrInvalidRefreshToken) {
		t.Error("the rotated-to token survived a detected replay")
	}
	if _, err := accounts.Refresh(ctx, other.RefreshToken); !errors.Is(err, domain.ErrInvalidRefreshToken) {
		t.Error("a sibling session survived a detected replay")
	}
}

// TestLogoutRevokesAndIsIdempotent: logging out twice is not an error, and a
// logged-out token cannot be refreshed.
func TestLogoutRevokesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	accounts := newAccounts(t, 15*time.Minute)
	email := uniqueEmail(t)
	const password = "correct-horse-battery-staple"

	if _, err := accounts.Register(ctx, email, password, ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	session, err := accounts.Login(ctx, email, password)
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	revoked, err := accounts.Logout(ctx, session.RefreshToken)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	if !revoked {
		t.Error("logout reported nothing to revoke on a live token")
	}

	// Retried logout: success, and it reports that it changed nothing. A retry
	// after a dropped response must not look like a failure to the client.
	revoked, err = accounts.Logout(ctx, session.RefreshToken)
	if err != nil {
		t.Fatalf("repeated logout: %v", err)
	}
	if revoked {
		t.Error("a repeated logout claimed to revoke a live token")
	}

	if _, err := accounts.Refresh(ctx, session.RefreshToken); !errors.Is(err, domain.ErrInvalidRefreshToken) {
		t.Fatalf("refresh after logout error = %v, want ErrInvalidRefreshToken", err)
	}
}

// TestLogoutDoesNotInvalidateTheAccessToken pins the trade documented in
// ARCHITECTURE.md §3.3: the gateway validates locally, so a logged-out access
// token stays verifiable until it expires. If this ever starts failing, either
// a revocation mechanism was added (update the docs) or the token TTL story
// changed (update the reasoning).
func TestLogoutDoesNotInvalidateTheAccessToken(t *testing.T) {
	ctx := context.Background()
	accounts := newAccounts(t, 15*time.Minute)
	email := uniqueEmail(t)
	const password = "correct-horse-battery-staple"

	user, err := accounts.Register(ctx, email, password, "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	session, err := accounts.Login(ctx, email, password)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := accounts.Logout(ctx, session.RefreshToken); err != nil {
		t.Fatalf("logout: %v", err)
	}
	assertVerifiableToken(t, session.AccessToken, user.ID, string(domain.RoleAttendee))
}

// TestExpiredAccessTokenIsRejectedByItsOwnClaims: the bound on that window is
// exp, so exp has to actually be there and actually be short.
func TestExpiredAccessTokenIsRejectedByItsOwnClaims(t *testing.T) {
	ctx := context.Background()
	// One nanosecond: expired before the assertion runs.
	accounts := newAccounts(t, time.Nanosecond)
	email := uniqueEmail(t)
	const password = "correct-horse-battery-staple"

	if _, err := accounts.Register(ctx, email, password, ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	session, err := accounts.Login(ctx, email, password)
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	_, err = jwt.Parse(session.AccessToken, func(*jwt.Token) (any, error) { return []byte(testSecret), nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if !errors.Is(err, jwt.ErrTokenExpired) {
		t.Fatalf("parse error = %v, want ErrTokenExpired", err)
	}
}

// assertVerifiableToken checks a minted access token the way the gateway does:
// pinned algorithm, matching issuer and audience, and the expected subject and
// role. It is the contract between the two services, asserted from the Auth side.
func assertVerifiableToken(t *testing.T, raw, wantUserID, wantRole string) {
	t.Helper()
	var claims struct {
		Role string `json:"role"`
		jwt.RegisteredClaims
	}
	if _, err := jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) { return []byte(testSecret), nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(testIssuer),
		jwt.WithAudience(testAudience),
		jwt.WithExpirationRequired(),
	); err != nil {
		t.Fatalf("access token did not verify: %v", err)
	}
	if claims.Subject != wantUserID {
		t.Errorf("sub = %q, want %q", claims.Subject, wantUserID)
	}
	if claims.Role != wantRole {
		t.Errorf("role = %q, want %q", claims.Role, wantRole)
	}
}
