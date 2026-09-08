// Package usecase holds application services: orchestration and transaction
// boundaries. It depends on domain and on the repository *interfaces* declared
// below, never on their implementations or on a database driver (AGENTS.md §4).
package usecase

import (
	"context"
	"time"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
)

// TxManager opens a transaction boundary. Transaction boundaries are owned by
// usecase, so the decision about what must commit together lives beside the
// business rule that requires it, not inside a repository or a handler.
type TxManager interface {
	// WithinTx runs fn in one transaction, committing if fn returns nil.
	// Implementations pass a context that carries the transaction; repository
	// calls made with that context join it.
	WithinTx(ctx context.Context, fn func(context.Context) error) error
}

// SeatRepository is the seat-state port.
type SeatRepository interface {
	// LockSeatsForUpdate takes row-level write locks on the given seats, in
	// sorted seat-ID order, and returns their state as of the lock. Only
	// meaningful inside a transaction.
	LockSeatsForUpdate(ctx context.Context, eventID string, seatIDs []string) ([]domain.Seat, error)
	// MarkSeatsHeld flips seats to held and reports how many rows changed.
	MarkSeatsHeld(ctx context.Context, seatIDs []string, reservationID string, until time.Time) (int64, error)
	// ListSeatsByEvent returns the seat map for an event.
	ListSeatsByEvent(ctx context.Context, eventID string) ([]domain.Seat, error)
	// ReleaseExpiredHolds frees seats whose checkout window closed and marks
	// their reservations expired. Returns the number of seats freed.
	ReleaseExpiredHolds(ctx context.Context) (int64, error)
}

// ReservationRepository is the reservation port.
type ReservationRepository interface {
	CreateReservation(ctx context.Context, res domain.Reservation) error
	AttachSeats(ctx context.Context, reservationID string, seatIDs []string) error
	// FindReservationByIdempotencyKey returns domain.ErrReservationNotFound
	// when the key has not been seen before.
	FindReservationByIdempotencyKey(ctx context.Context, key string) (domain.Reservation, error)
}

// EventRepository is the catalog port. P0 needs only enough of it to seed a
// demo event and its seat map.
type EventRepository interface {
	CreateEvent(ctx context.Context, ev domain.Event) error
	CreateSeats(ctx context.Context, seats []domain.Seat) error
}
