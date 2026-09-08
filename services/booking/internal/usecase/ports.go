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
	// their reservations expired. Returns the number of seats freed. It skips
	// reservations whose payment outcome is unknown (D8).
	ReleaseExpiredHolds(ctx context.Context) (int64, error)

	// LockSeatsByReservation locks the seats a reservation actively claims, in
	// sorted seat-ID order (D5). Only meaningful inside a transaction.
	LockSeatsByReservation(ctx context.Context, reservationID string) ([]domain.Seat, error)
	// MarkSeatsBooked flips a reservation's held seats to booked.
	MarkSeatsBooked(ctx context.Context, reservationID string) (int64, error)
	// ReleaseHeldSeats returns a reservation's held seats to available and
	// stamps its claims released.
	ReleaseHeldSeats(ctx context.Context, reservationID string) (int64, error)
	// ReleaseBookedSeats returns a reservation's booked seats to available,
	// for a refund.
	ReleaseBookedSeats(ctx context.Context, reservationID string) (int64, error)
}

// ReservationRepository is the reservation port.
type ReservationRepository interface {
	CreateReservation(ctx context.Context, res domain.Reservation) error
	AttachSeats(ctx context.Context, reservationID string, seatIDs []string) error
	// FindReservationByIdempotencyKey returns domain.ErrReservationNotFound
	// when the key has not been seen before.
	FindReservationByIdempotencyKey(ctx context.Context, key string) (domain.Reservation, error)

	// GetReservation reads a reservation without locking it.
	GetReservation(ctx context.Context, id string) (domain.Reservation, error)
	// LockReservation takes the reservation's write lock and returns its state
	// as of the lock. Only meaningful inside a transaction, and always taken
	// before the seats it owns.
	LockReservation(ctx context.Context, id string) (domain.Reservation, error)
	// SetReservationStatus moves a reservation between two states, reporting
	// whether the row was still in the from state. False means a concurrent
	// writer got there first; the caller lost and must not assume otherwise.
	SetReservationStatus(ctx context.Context, id string, from, to domain.ReservationStatus) (bool, error)
	// MarkPaymentPending records that a charge may exist for this reservation
	// whose outcome nobody knows, handing it to the reconciliation job (D8).
	MarkPaymentPending(ctx context.Context, id string) (bool, error)
	// ListReservationsAwaitingReconciliation returns pending reservations whose
	// payment outcome has been unknown since before olderThan.
	ListReservationsAwaitingReconciliation(ctx context.Context, olderThan time.Time, limit int) ([]domain.Reservation, error)
}

// BookingRepository is the confirmed-purchase port.
type BookingRepository interface {
	// CreateBooking records a completed purchase. It returns
	// domain.ErrBookingExists if the reservation already has one - the
	// database-level half of confirmation idempotency.
	CreateBooking(ctx context.Context, b domain.Booking) error
	// GetBookingByReservation returns domain.ErrBookingNotFound when the
	// reservation has not been confirmed.
	GetBookingByReservation(ctx context.Context, reservationID string) (domain.Booking, error)
	// SetBookingStatus moves a booking between two states, reporting whether
	// the row was still in the from state.
	SetBookingStatus(ctx context.Context, id string, from, to domain.BookingStatus) (bool, error)
	CreateTickets(ctx context.Context, tickets []domain.Ticket) error
	ListTicketsByBooking(ctx context.Context, bookingID string) ([]domain.Ticket, error)
}

// EventRepository is the catalog port. P0 needs only enough of it to seed a
// demo event and its seat map.
type EventRepository interface {
	CreateEvent(ctx context.Context, ev domain.Event) error
	CreateSeats(ctx context.Context, seats []domain.Seat) error
}

// ChargeRequest is one request for Payment to take money.
type ChargeRequest struct {
	ReservationID  string
	UserID         string
	AmountCents    int64
	IdempotencyKey string
}

// PaymentClient is the port onto the Payment service: the only synchronous
// service-to-service call in the system (ARCHITECTURE.md §4.1).
//
// The contract that matters is the error contract. Every method returns
// domain.ErrPaymentOutcomeUnknown - and only that - when the call did not
// complete: a timeout, a dropped connection, an open circuit breaker. That
// error means the outcome is unknown, never that it failed, and no caller may
// release a seat on it (D8).
type PaymentClient interface {
	// Charge asks Payment to take money. A settled result is returned as a
	// value; an unknown one as domain.ErrPaymentOutcomeUnknown.
	Charge(ctx context.Context, req ChargeRequest) (domain.PaymentResult, error)
	// GetCharge asks what happened to the charge under an idempotency key.
	// A key Payment has never seen comes back as domain.PaymentNoCharge, which
	// is the only answer that makes releasing seats safe after a timeout.
	GetCharge(ctx context.Context, idempotencyKey string) (domain.PaymentResult, error)
	// Refund gives money back against a charge.
	Refund(ctx context.Context, chargeID string, amountCents int64, idempotencyKey string) error
}
