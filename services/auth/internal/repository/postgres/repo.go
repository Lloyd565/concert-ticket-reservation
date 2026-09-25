// Package postgres implements the repository interfaces declared in usecase.
// It is the only place in the service that imports a database driver
// (AGENTS.md §4). Domain entities carry string IDs; parsing to and from uuid
// happens here, at the boundary, so domain stays dependency-free.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/domain"
)

// uniqueViolation is the SQLSTATE for a unique-index conflict. Matching on the
// code rather than the message keeps the mapping stable across Postgres
// versions and locales.
const uniqueViolation = "23505"

// Repo implements usecase.UserRepository and usecase.RefreshTokenRepository.
//
// There is no TxManager here. Auth has no multi-statement invariant to protect:
// every operation below is a single statement, and single statements are
// already atomic. Booking needs transaction boundaries because it holds locks
// across a check and an act; Auth does not, and inventing one would be
// ceremony.
type Repo struct {
	queries *Queries
}

// NewRepo returns a Repo backed by pool.
func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{queries: New(pool)} }

// CreateUser inserts an account, mapping the email UNIQUE violation to a domain
// error so the boundary can answer AlreadyExists instead of Internal.
func (r *Repo) CreateUser(ctx context.Context, u domain.User) error {
	id, err := uuid.Parse(u.ID)
	if err != nil {
		return fmt.Errorf("parse user id %q: %w", u.ID, domain.ErrInvalidInput)
	}
	err = r.queries.CreateUser(ctx, CreateUserParams{
		ID:           id,
		Email:        u.Email,
		PasswordHash: u.PasswordHash,
		Role:         string(u.Role),
		CreatedAt:    u.CreatedAt,
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return domain.ErrEmailTaken
	}
	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

// AdminExists reports whether any account holds the admin role. It backs the
// startup bootstrap's idempotency check.
func (r *Repo) AdminExists(ctx context.Context) (bool, error) {
	exists, err := r.queries.AdminExists(ctx)
	if err != nil {
		return false, fmt.Errorf("check for an existing admin: %w", err)
	}
	return exists, nil
}

// FindUserByEmail looks an account up by its normalised address.
func (r *Repo) FindUserByEmail(ctx context.Context, email string) (domain.User, error) {
	row, err := r.queries.GetUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, domain.ErrUserNotFound
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("select user by email: %w", err)
	}
	return toDomainUser(row), nil
}

// FindUserByID looks an account up by ID.
func (r *Repo) FindUserByID(ctx context.Context, id string) (domain.User, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return domain.User{}, fmt.Errorf("parse user id %q: %w", id, domain.ErrInvalidInput)
	}
	row, err := r.queries.GetUserByID(ctx, parsed)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, domain.ErrUserNotFound
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("select user by id: %w", err)
	}
	return toDomainUser(row), nil
}

// StoreRefreshToken records the hash of a newly issued refresh token.
func (r *Repo) StoreRefreshToken(ctx context.Context, id, userID string, hash []byte, expiresAt time.Time) error {
	tokenID, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("parse token id %q: %w", id, domain.ErrInvalidInput)
	}
	owner, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("parse user id %q: %w", userID, domain.ErrInvalidInput)
	}
	if err := r.queries.CreateRefreshToken(ctx, CreateRefreshTokenParams{
		ID:        tokenID,
		UserID:    owner,
		TokenHash: hash,
		ExpiresAt: expiresAt,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("insert refresh token: %w", err)
	}
	return nil
}

// FindRefreshToken returns a stored token by its hash. Expired and revoked
// tokens are returned rather than hidden: usecase needs to tell "never existed"
// from "was rotated away", because the second is evidence of a replay.
func (r *Repo) FindRefreshToken(ctx context.Context, hash []byte) (domain.RefreshToken, error) {
	row, err := r.queries.GetRefreshToken(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RefreshToken{}, domain.ErrInvalidRefreshToken
	}
	if err != nil {
		return domain.RefreshToken{}, fmt.Errorf("select refresh token: %w", err)
	}
	return domain.RefreshToken{
		ID:        row.ID.String(),
		UserID:    row.UserID.String(),
		ExpiresAt: row.ExpiresAt,
		RevokedAt: row.RevokedAt,
	}, nil
}

// RevokeRefreshToken stamps revoked_at on a live token and reports whether it
// changed anything. The "no rows" case is the ordinary outcome of a retried
// logout, not an error.
func (r *Repo) RevokeRefreshToken(ctx context.Context, hash []byte) (bool, error) {
	_, err := r.queries.RevokeRefreshToken(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("revoke refresh token: %w", err)
	}
	return true, nil
}

// RevokeAllForUser cuts every live session for one account.
func (r *Repo) RevokeAllForUser(ctx context.Context, userID string) error {
	owner, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("parse user id %q: %w", userID, domain.ErrInvalidInput)
	}
	if err := r.queries.RevokeUserRefreshTokens(ctx, owner); err != nil {
		return fmt.Errorf("revoke user refresh tokens: %w", err)
	}
	return nil
}

// PurgeExpiredRefreshTokens deletes tokens past their expiry. Revocation is
// what makes a token unusable; this only keeps the table from growing forever.
func (r *Repo) PurgeExpiredRefreshTokens(ctx context.Context) error {
	if err := r.queries.DeleteExpiredRefreshTokens(ctx); err != nil {
		return fmt.Errorf("delete expired refresh tokens: %w", err)
	}
	return nil
}

func toDomainUser(row User) domain.User {
	return domain.User{
		ID:           row.ID.String(),
		Email:        row.Email,
		PasswordHash: row.PasswordHash,
		Role:         domain.Role(row.Role),
		CreatedAt:    row.CreatedAt,
	}
}
