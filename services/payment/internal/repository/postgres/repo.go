// Package postgres implements the repository interfaces declared in usecase.
// It is the only place in the service that imports a database driver
// (AGENTS.md §4). Domain entities carry string IDs; parsing to and from uuid
// happens here, at the boundary, so domain stays dependency-free.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/domain"
)

// uniqueViolation is the SQLSTATE for a unique-index conflict. Matching on the
// code rather than the message keeps the mapping stable across Postgres
// versions and locales.
const uniqueViolation = "23505"

// Repo implements usecase.ChargeRepository and usecase.RefundRepository.
//
// There is no TxManager here. Every operation below is a single statement, and
// single statements are already atomic; Booking needs transaction boundaries
// because it holds locks across a check and an act, Payment does not.
type Repo struct{ queries *Queries }

// NewRepo returns a Repo backed by pool.
func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{queries: New(pool)} }

// CreateCharge inserts a pending charge, mapping the idempotency-key UNIQUE
// violation to a domain error so the use case can join the winning charge
// instead of reporting an internal failure.
func (r *Repo) CreateCharge(ctx context.Context, c domain.Charge) error {
	id, resID, userID, err := chargeIDs(c)
	if err != nil {
		return err
	}
	err = r.queries.CreateCharge(ctx, CreateChargeParams{
		ID:             id,
		ReservationID:  resID,
		UserID:         userID,
		AmountCents:    c.AmountCents,
		Status:         string(c.Status),
		IdempotencyKey: c.IdempotencyKey,
		CreatedAt:      c.CreatedAt,
		UpdatedAt:      c.UpdatedAt,
	})
	if isUniqueViolation(err) {
		return domain.ErrChargeExists
	}
	if err != nil {
		return fmt.Errorf("create charge: %w", err)
	}
	return nil
}

// FindChargeByIdempotencyKey returns the charge recorded under key.
func (r *Repo) FindChargeByIdempotencyKey(ctx context.Context, key string) (domain.Charge, error) {
	row, err := r.queries.GetChargeByIdempotencyKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Charge{}, domain.ErrChargeNotFound
	}
	if err != nil {
		return domain.Charge{}, fmt.Errorf("find charge by idempotency key: %w", err)
	}
	return toCharge(row), nil
}

// FindChargeByID returns the charge with the given id.
func (r *Repo) FindChargeByID(ctx context.Context, id string) (domain.Charge, error) {
	chargeID, err := uuid.Parse(id)
	if err != nil {
		return domain.Charge{}, fmt.Errorf("parse charge id %q: %w", id, domain.ErrInvalidInput)
	}
	row, err := r.queries.GetChargeByID(ctx, chargeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Charge{}, domain.ErrChargeNotFound
	}
	if err != nil {
		return domain.Charge{}, fmt.Errorf("find charge by id: %w", err)
	}
	return toCharge(row), nil
}

// SettleCharge records a terminal outcome, reporting whether the row was still
// pending and therefore whether this result is the authoritative one.
func (r *Repo) SettleCharge(ctx context.Context, c domain.Charge) (bool, error) {
	id, err := uuid.Parse(c.ID)
	if err != nil {
		return false, fmt.Errorf("parse charge id %q: %w", c.ID, domain.ErrInvalidInput)
	}
	n, err := r.queries.SettleCharge(ctx, SettleChargeParams{
		Status:        string(c.Status),
		ProviderRef:   nullable(c.ProviderRef),
		DeclineReason: nullable(c.DeclineReason),
		UpdatedAt:     c.UpdatedAt,
		ID:            id,
	})
	if err != nil {
		return false, fmt.Errorf("settle charge: %w", err)
	}
	return n == 1, nil
}

// CreateRefund inserts a pending refund.
func (r *Repo) CreateRefund(ctx context.Context, rf domain.Refund) error {
	id, err := uuid.Parse(rf.ID)
	if err != nil {
		return fmt.Errorf("parse refund id %q: %w", rf.ID, domain.ErrInvalidInput)
	}
	chargeID, err := uuid.Parse(rf.ChargeID)
	if err != nil {
		return fmt.Errorf("parse charge id %q: %w", rf.ChargeID, domain.ErrInvalidInput)
	}
	err = r.queries.CreateRefund(ctx, CreateRefundParams{
		ID:             id,
		ChargeID:       chargeID,
		AmountCents:    rf.AmountCents,
		Status:         string(rf.Status),
		IdempotencyKey: rf.IdempotencyKey,
		CreatedAt:      rf.CreatedAt,
		UpdatedAt:      rf.UpdatedAt,
	})
	if isUniqueViolation(err) {
		return domain.ErrRefundExists
	}
	if err != nil {
		return fmt.Errorf("create refund: %w", err)
	}
	return nil
}

// FindRefundByIdempotencyKey returns the refund recorded under key.
func (r *Repo) FindRefundByIdempotencyKey(ctx context.Context, key string) (domain.Refund, error) {
	row, err := r.queries.GetRefundByIdempotencyKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Refund{}, domain.ErrRefundNotFound
	}
	if err != nil {
		return domain.Refund{}, fmt.Errorf("find refund by idempotency key: %w", err)
	}
	return toRefund(row), nil
}

// SettleRefund records a terminal outcome, reporting whether the row was still
// pending.
func (r *Repo) SettleRefund(ctx context.Context, rf domain.Refund) (bool, error) {
	id, err := uuid.Parse(rf.ID)
	if err != nil {
		return false, fmt.Errorf("parse refund id %q: %w", rf.ID, domain.ErrInvalidInput)
	}
	n, err := r.queries.SettleRefund(ctx, SettleRefundParams{
		Status:      string(rf.Status),
		ProviderRef: nullable(rf.ProviderRef),
		UpdatedAt:   rf.UpdatedAt,
		ID:          id,
	})
	if err != nil {
		return false, fmt.Errorf("settle refund: %w", err)
	}
	return n == 1, nil
}

func toCharge(row Charge) domain.Charge {
	return domain.Charge{
		ID:             row.ID.String(),
		ReservationID:  row.ReservationID.String(),
		UserID:         row.UserID.String(),
		AmountCents:    row.AmountCents,
		Status:         domain.ChargeStatus(row.Status),
		ProviderRef:    deref(row.ProviderRef),
		DeclineReason:  deref(row.DeclineReason),
		IdempotencyKey: row.IdempotencyKey,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}

func toRefund(row Refund) domain.Refund {
	return domain.Refund{
		ID:             row.ID.String(),
		ChargeID:       row.ChargeID.String(),
		AmountCents:    row.AmountCents,
		Status:         domain.RefundStatus(row.Status),
		ProviderRef:    deref(row.ProviderRef),
		IdempotencyKey: row.IdempotencyKey,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}

func chargeIDs(c domain.Charge) (id, reservationID, userID uuid.UUID, err error) {
	if id, err = uuid.Parse(c.ID); err != nil {
		return id, reservationID, userID, fmt.Errorf("parse charge id %q: %w", c.ID, domain.ErrInvalidInput)
	}
	if reservationID, err = uuid.Parse(c.ReservationID); err != nil {
		return id, reservationID, userID, fmt.Errorf("parse reservation id %q: %w", c.ReservationID, domain.ErrInvalidInput)
	}
	if userID, err = uuid.Parse(c.UserID); err != nil {
		return id, reservationID, userID, fmt.Errorf("parse user id %q: %w", c.UserID, domain.ErrInvalidInput)
	}
	return id, reservationID, userID, nil
}

// nullable maps the domain's empty string to SQL NULL. The columns are nullable
// because "no provider reference yet" is a real state, and an empty string
// pretending to be one would defeat the charges_settled_has_ref constraint.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// isUniqueViolation reports whether err is a Postgres 23505.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}
