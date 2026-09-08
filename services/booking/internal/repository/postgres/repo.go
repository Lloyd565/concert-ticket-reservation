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

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
)

// txKey carries the ambient transaction. usecase opens the boundary through
// TxManager and never sees a pgx type; repository methods then pick the
// transaction up from the context so every write inside WithinTx lands in the
// same transaction as the SELECT ... FOR UPDATE that guarded it.
type txKey struct{}

// TxManager implements usecase.TxManager.
type TxManager struct{ pool *pgxpool.Pool }

// NewTxManager returns a TxManager bound to pool.
func NewTxManager(pool *pgxpool.Pool) *TxManager { return &TxManager{pool: pool} }

// WithinTx runs fn inside a single database transaction, committing if fn
// returns nil and rolling back otherwise.
//
// The transaction ends when this function returns. No external call may be made
// from inside fn (D4): a lock held across a network hop turns a 5 ms
// transaction into a multi-second one under load.
func (m *TxManager) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	// Rollback after a successful Commit is a no-op, so this is safe as the
	// single cleanup path for panics and early returns alike.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// Repo implements the seat, reservation and event repository interfaces.
type Repo struct {
	pool    *pgxpool.Pool
	queries *Queries
}

// NewRepo returns a Repo backed by pool.
func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool, queries: New(pool)} }

// q returns the querier bound to the ambient transaction, or the pool-backed
// querier for calls made outside one.
func (r *Repo) q(ctx context.Context) *Queries {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return r.queries.WithTx(tx)
	}
	return r.queries
}

// LockSeatsForUpdate locks the given seats with SELECT ... FOR UPDATE, ordered
// by seat ID, and returns their current state. Must be called inside a
// transaction; the lock is released when that transaction ends.
func (r *Repo) LockSeatsForUpdate(ctx context.Context, eventID string, seatIDs []string) ([]domain.Seat, error) {
	evID, ids, err := parseIDs(eventID, seatIDs)
	if err != nil {
		return nil, err
	}
	rows, err := r.q(ctx).LockSeatsForUpdate(ctx, LockSeatsForUpdateParams{EventID: evID, SeatIds: ids})
	if err != nil {
		return nil, fmt.Errorf("lock seats for update: %w", err)
	}
	seats := make([]domain.Seat, 0, len(rows))
	for _, row := range rows {
		seats = append(seats, toSeat(row.ID, row.EventID, row.Section, row.Row, row.Number, row.Status, row.HeldByReservation, row.HeldUntil))
	}
	return seats, nil
}

// MarkSeatsHeld flips locked seats to held and returns the number updated.
func (r *Repo) MarkSeatsHeld(ctx context.Context, seatIDs []string, reservationID string, until time.Time) (int64, error) {
	resID, err := uuid.Parse(reservationID)
	if err != nil {
		return 0, fmt.Errorf("parse reservation id: %w", err)
	}
	ids, err := parseSeatIDs(seatIDs)
	if err != nil {
		return 0, err
	}
	n, err := r.q(ctx).MarkSeatsHeld(ctx, MarkSeatsHeldParams{ReservationID: &resID, HeldUntil: &until, SeatIds: ids})
	if err != nil {
		return 0, fmt.Errorf("mark seats held: %w", err)
	}
	return n, nil
}

// ListSeatsByEvent returns the full seat map for an event.
func (r *Repo) ListSeatsByEvent(ctx context.Context, eventID string) ([]domain.Seat, error) {
	evID, err := uuid.Parse(eventID)
	if err != nil {
		return nil, fmt.Errorf("parse event id: %w", err)
	}
	rows, err := r.q(ctx).ListSeatsByEvent(ctx, evID)
	if err != nil {
		return nil, fmt.Errorf("list seats: %w", err)
	}
	seats := make([]domain.Seat, 0, len(rows))
	for _, row := range rows {
		seats = append(seats, toSeat(row.ID, row.EventID, row.Section, row.Row, row.Number, row.Status, row.HeldByReservation, row.HeldUntil))
	}
	return seats, nil
}

// CreateReservation inserts a reservation row.
func (r *Repo) CreateReservation(ctx context.Context, res domain.Reservation) error {
	id, err := uuid.Parse(res.ID)
	if err != nil {
		return fmt.Errorf("parse reservation id: %w", err)
	}
	userID, err := uuid.Parse(res.UserID)
	if err != nil {
		return fmt.Errorf("parse user id: %w", err)
	}
	eventID, err := uuid.Parse(res.EventID)
	if err != nil {
		return fmt.Errorf("parse event id: %w", err)
	}
	err = r.q(ctx).CreateReservation(ctx, CreateReservationParams{
		ID:             id,
		UserID:         userID,
		EventID:        eventID,
		Status:         string(res.Status),
		ExpiresAt:      res.ExpiresAt,
		IdempotencyKey: res.IdempotencyKey,
	})
	if isUniqueViolation(err, "reservations_idempotency_key_key") {
		// Two identical keys raced. The winner's reservation is authoritative;
		// this transaction is poisoned, so the caller retries against it.
		// ponytail: reported as a conflict rather than resolved here. Resolving
		// it needs a fresh transaction to re-read the winner - add that if
		// concurrent duplicate keys ever show up in practice.
		return domain.ErrDuplicateRequest
	}
	if err != nil {
		return fmt.Errorf("create reservation: %w", err)
	}
	return nil
}

// AttachSeats records the reservation's claim on each seat.
func (r *Repo) AttachSeats(ctx context.Context, reservationID string, seatIDs []string) error {
	resID, err := uuid.Parse(reservationID)
	if err != nil {
		return fmt.Errorf("parse reservation id: %w", err)
	}
	ids, err := parseSeatIDs(seatIDs)
	if err != nil {
		return err
	}
	err = r.q(ctx).AttachSeatsToReservation(ctx, AttachSeatsToReservationParams{ReservationID: resID, SeatIds: ids})
	if isUniqueViolation(err, "one_active_claim_per_seat") {
		// The D13 backstop fired: the application believed these seats were
		// free while another live claim existed. Reported as unavailable so the
		// client still gets a correct 409 - but reaching this line means the
		// locking path above it has a bug.
		return fmt.Errorf("attach seats to reservation %s: %w", reservationID, domain.ErrBackstopTripped)
	}
	if err != nil {
		return fmt.Errorf("attach seats to reservation: %w", err)
	}
	return nil
}

// FindReservationByIdempotencyKey returns a previously created reservation and
// its seats, or domain.ErrReservationNotFound.
func (r *Repo) FindReservationByIdempotencyKey(ctx context.Context, key string) (domain.Reservation, error) {
	row, err := r.q(ctx).GetReservationByIdempotencyKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Reservation{}, domain.ErrReservationNotFound
	}
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("find reservation by idempotency key: %w", err)
	}
	seatIDs, err := r.q(ctx).GetReservationSeatIDs(ctx, row.ID)
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("load reservation seats: %w", err)
	}
	res := domain.Reservation{
		ID:             row.ID.String(),
		UserID:         row.UserID.String(),
		EventID:        row.EventID.String(),
		Status:         domain.ReservationStatus(row.Status),
		ExpiresAt:      row.ExpiresAt,
		IdempotencyKey: row.IdempotencyKey,
		CreatedAt:      row.CreatedAt,
	}
	for _, id := range seatIDs {
		res.SeatIDs = append(res.SeatIDs, id.String())
	}
	return res, nil
}

// ReleaseExpiredHolds returns seats whose checkout window closed to available
// and marks their reservations expired. Returns the number of seats freed.
func (r *Repo) ReleaseExpiredHolds(ctx context.Context) (int64, error) {
	n, err := r.q(ctx).ReleaseExpiredHolds(ctx)
	if err != nil {
		return 0, fmt.Errorf("release expired holds: %w", err)
	}
	return n, nil
}

// CreateEvent inserts an event.
func (r *Repo) CreateEvent(ctx context.Context, ev domain.Event) error {
	id, err := uuid.Parse(ev.ID)
	if err != nil {
		return fmt.Errorf("parse event id: %w", err)
	}
	if err := r.q(ctx).CreateEvent(ctx, CreateEventParams{ID: id, Name: ev.Name, StartsAt: ev.StartsAt}); err != nil {
		return fmt.Errorf("create event: %w", err)
	}
	return nil
}

// CreateSeats bulk-inserts a seat map.
func (r *Repo) CreateSeats(ctx context.Context, seats []domain.Seat) error {
	params := make([]CreateSeatsParams, 0, len(seats))
	for _, s := range seats {
		id, err := uuid.Parse(s.ID)
		if err != nil {
			return fmt.Errorf("parse seat id: %w", err)
		}
		eventID, err := uuid.Parse(s.EventID)
		if err != nil {
			return fmt.Errorf("parse event id: %w", err)
		}
		params = append(params, CreateSeatsParams{ID: id, EventID: eventID, Section: s.Section, Row: s.Row, Number: s.Number})
	}
	if _, err := r.q(ctx).CreateSeats(ctx, params); err != nil {
		return fmt.Errorf("create seats: %w", err)
	}
	return nil
}

func toSeat(id, eventID uuid.UUID, section, row, number, status string, heldBy *uuid.UUID, heldUntil *time.Time) domain.Seat {
	s := domain.Seat{
		ID:        id.String(),
		EventID:   eventID.String(),
		Section:   section,
		Row:       row,
		Number:    number,
		Status:    domain.SeatStatus(status),
		HeldUntil: heldUntil,
	}
	if heldBy != nil {
		s.HeldBy = heldBy.String()
	}
	return s
}

func parseIDs(eventID string, seatIDs []string) (uuid.UUID, []uuid.UUID, error) {
	evID, err := uuid.Parse(eventID)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("parse event id: %w", err)
	}
	ids, err := parseSeatIDs(seatIDs)
	if err != nil {
		return uuid.Nil, nil, err
	}
	return evID, ids, nil
}

func parseSeatIDs(seatIDs []string) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, len(seatIDs))
	for _, s := range seatIDs {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("parse seat id %q: %w", s, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// isUniqueViolation reports whether err is a Postgres 23505 raised by the named
// constraint or index.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
