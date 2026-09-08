package usecase

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/token"
)

// Accounts is the Auth service's application logic: registration, login,
// refresh and logout.
type Accounts struct {
	users      UserRepository
	sessions   RefreshTokenRepository
	signer     *token.Signer
	refreshTTL time.Duration
}

// NewAccounts wires an Accounts service. refreshTTL is how long a refresh token
// stays exchangeable.
func NewAccounts(users UserRepository, sessions RefreshTokenRepository, signer *token.Signer, refreshTTL time.Duration) *Accounts {
	return &Accounts{users: users, sessions: sessions, signer: signer, refreshTTL: refreshTTL}
}

// Session is a freshly minted credential pair plus the identity it belongs to.
type Session struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    time.Duration
	UserID       string
	Role         domain.Role
}

// Register creates an account. The password is hashed with Argon2id before it
// reaches the repository; the plaintext never leaves this function.
func (a *Accounts) Register(ctx context.Context, email, password, role string) (domain.User, error) {
	normalized := domain.NormalizeEmail(email)
	if !domain.ValidEmail(normalized) {
		return domain.User{}, fmt.Errorf("register: %q is not an email address: %w", email, domain.ErrInvalidInput)
	}
	if len(password) < domain.MinPasswordLength {
		return domain.User{}, fmt.Errorf("register: %w (minimum %d characters)", domain.ErrWeakPassword, domain.MinPasswordLength)
	}
	parsedRole, err := domain.ParseRole(role)
	if err != nil {
		return domain.User{}, fmt.Errorf("register: %w", err)
	}

	hash, err := HashPassword(password)
	if err != nil {
		return domain.User{}, fmt.Errorf("register: %w", err)
	}
	user := domain.User{
		ID:           uuid.Must(uuid.NewV7()).String(),
		Email:        normalized,
		PasswordHash: hash,
		Role:         parsedRole,
		CreatedAt:    time.Now().UTC(),
	}
	if err := a.users.CreateUser(ctx, user); err != nil {
		return domain.User{}, fmt.Errorf("register: %w", err)
	}
	return user, nil
}

// Login exchanges credentials for a token pair.
func (a *Accounts) Login(ctx context.Context, email, password string) (Session, error) {
	user, err := a.users.FindUserByEmail(ctx, domain.NormalizeEmail(email))
	switch {
	case errors.Is(err, domain.ErrUserNotFound):
		// Hash anyway before answering. Returning early here makes "no such
		// user" measurably faster than "wrong password", which turns login into
		// an account-enumeration oracle for anyone with a stopwatch.
		_, _ = HashPassword(password)
		return Session{}, fmt.Errorf("login: %w", domain.ErrInvalidCredentials)
	case err != nil:
		return Session{}, fmt.Errorf("login: %w", err)
	}
	if !VerifyPassword(user.PasswordHash, password) {
		return Session{}, fmt.Errorf("login: %w", domain.ErrInvalidCredentials)
	}
	return a.issue(ctx, user)
}

// Refresh exchanges a live refresh token for a new pair and revokes the old one.
//
// Rotation is not optional here: without it a stolen refresh token is a
// permanent session for its full lifetime and there is no way to tell a theft
// from normal use. With it, the legitimate client and a thief cannot both keep
// using the chain, and the second presentation of an already-rotated token is
// detectable - treated below as the compromise it probably is.
func (a *Accounts) Refresh(ctx context.Context, refreshToken string) (Session, error) {
	if refreshToken == "" {
		return Session{}, fmt.Errorf("refresh: %w", domain.ErrInvalidRefreshToken)
	}
	hash := hashRefreshToken(refreshToken)

	stored, err := a.sessions.FindRefreshToken(ctx, hash)
	if err != nil {
		return Session{}, fmt.Errorf("refresh: %w", err)
	}
	if !stored.Live(time.Now().UTC()) {
		// A known-but-revoked token being presented means somebody replayed a
		// token that was already rotated away. Either the client raced itself
		// or the chain leaked; the safe reading is the second, so every live
		// session for this user is cut and they must log in again.
		if stored.RevokedAt != nil {
			if err := a.sessions.RevokeAllForUser(ctx, stored.UserID); err != nil {
				return Session{}, fmt.Errorf("refresh: revoke reused token chain: %w", err)
			}
		}
		return Session{}, fmt.Errorf("refresh: %w", domain.ErrInvalidRefreshToken)
	}

	user, err := a.users.FindUserByID(ctx, stored.UserID)
	if err != nil {
		// The account went away between issuing and refreshing. This is the
		// mechanism that bounds the gateway's local-validation window: the
		// session dies here, at the next refresh, with no per-request call.
		if errors.Is(err, domain.ErrUserNotFound) {
			return Session{}, fmt.Errorf("refresh: %w", domain.ErrInvalidRefreshToken)
		}
		return Session{}, fmt.Errorf("refresh: %w", err)
	}

	// Revoke before issuing. If the new token were stored first and the process
	// died before the old one was revoked, two live tokens would exist; this
	// order merely costs the caller one retry in the same crash.
	revoked, err := a.sessions.RevokeRefreshToken(ctx, hash)
	if err != nil {
		return Session{}, fmt.Errorf("refresh: %w", err)
	}
	if !revoked {
		// Lost a race with a concurrent refresh of the same token. The other
		// caller has the new pair; this one must not get a second.
		return Session{}, fmt.Errorf("refresh: %w", domain.ErrInvalidRefreshToken)
	}
	return a.issue(ctx, user)
}

// Logout revokes a refresh token. It reports whether it revoked a live one;
// revoking an already-dead token is a success rather than an error, so a
// retried logout is safe (AGENTS.md §2 rule 10).
//
// The caller's access token stays valid until it expires: the gateway does not
// ask anyone about it (ARCHITECTURE.md §3.3). The access-token TTL is therefore
// the worst-case delay between logging out and the session actually being dead.
func (a *Accounts) Logout(ctx context.Context, refreshToken string) (bool, error) {
	if refreshToken == "" {
		return false, fmt.Errorf("logout: %w", domain.ErrInvalidRefreshToken)
	}
	revoked, err := a.sessions.RevokeRefreshToken(ctx, hashRefreshToken(refreshToken))
	if err != nil {
		return false, fmt.Errorf("logout: %w", err)
	}
	return revoked, nil
}

// issue mints an access/refresh pair for a user and records the refresh token's
// hash.
func (a *Accounts) issue(ctx context.Context, user domain.User) (Session, error) {
	now := time.Now().UTC()
	access, err := a.signer.Sign(user.ID, string(user.Role), now)
	if err != nil {
		return Session{}, err
	}
	refresh, err := token.NewRefreshToken()
	if err != nil {
		return Session{}, err
	}
	if err := a.sessions.StoreRefreshToken(ctx, uuid.Must(uuid.NewV7()).String(), user.ID, hashRefreshToken(refresh), now.Add(a.refreshTTL)); err != nil {
		return Session{}, fmt.Errorf("store refresh token: %w", err)
	}
	return Session{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    a.signer.TTL(),
		UserID:       user.ID,
		Role:         user.Role,
	}, nil
}

// hashRefreshToken is the lookup key for a refresh token. SHA-256 and not
// Argon2id: the input is 256 bits of CSPRNG output, so there is no dictionary
// to slow down, and a per-request Argon2 pass would make refresh the most
// expensive operation in the system for no security gain.
func hashRefreshToken(t string) []byte {
	sum := sha256.Sum256([]byte(t))
	return sum[:]
}
