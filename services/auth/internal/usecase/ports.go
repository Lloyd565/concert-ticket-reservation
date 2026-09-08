// Package usecase holds the Auth service's application logic. It depends on
// domain and on the repository interfaces declared here, never on their
// implementations or on a database driver (AGENTS.md §4).
package usecase

import (
	"context"
	"time"

	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/domain"
)

// UserRepository is the account store port.
type UserRepository interface {
	// CreateUser returns domain.ErrEmailTaken when the address is already
	// registered.
	CreateUser(ctx context.Context, u domain.User) error
	// FindUserByEmail returns domain.ErrUserNotFound when there is no match.
	FindUserByEmail(ctx context.Context, email string) (domain.User, error)
	// FindUserByID returns domain.ErrUserNotFound when there is no match.
	FindUserByID(ctx context.Context, id string) (domain.User, error)
	// AdminExists reports whether any account holds the admin role.
	AdminExists(ctx context.Context) (bool, error)
}

// RefreshTokenRepository is the session store port. Tokens are addressed by
// their hash: the plaintext never reaches this layer's storage.
type RefreshTokenRepository interface {
	StoreRefreshToken(ctx context.Context, id, userID string, hash []byte, expiresAt time.Time) error
	// FindRefreshToken returns domain.ErrInvalidRefreshToken when the hash is
	// unknown.
	FindRefreshToken(ctx context.Context, hash []byte) (domain.RefreshToken, error)
	// RevokeRefreshToken reports whether it revoked a live token. False means
	// the token was already revoked, expired or never existed.
	RevokeRefreshToken(ctx context.Context, hash []byte) (bool, error)
	// RevokeAllForUser cuts every live session for a user.
	RevokeAllForUser(ctx context.Context, userID string) error
}
