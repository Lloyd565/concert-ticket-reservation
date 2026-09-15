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

// HoldStore is the seat-hold port: Redis in production (D3, ARCHITECTURE.md
// §5.2). It is the lock. Everything in it fails closed - a method that cannot
// reach the store returns domain.ErrHoldsUnavailable and claims nothing, and no
// caller may treat that as permission to proceed unlocked (D12).
type HoldStore interface {
	// Claim atomically claims every seat for reservationID for ttl, or claims
	// none (D6). A seat already held returns domain.ErrSeatUnavailable.
	Claim(ctx context.Context, eventID string, seatIDs []string, reservationID string, ttl time.Duration) error
	// Release drops the claims this reservation still owns, reporting how many
	// were dropped. A claim that has expired, or been re-taken by somebody
	// else, is left alone.
	Release(ctx context.Context, eventID string, seatIDs []string, reservationID string) (int, error)
	// Extend pushes the expiry of the claims this reservation still owns out to
	// ttl from now. This is how D8 survives a TTL: seats under an unknown
	// payment outcome must not go back on sale on their own.
	Extend(ctx context.Context, eventID string, seatIDs []string, reservationID string, ttl time.Duration) (int, error)
	// Owns reports whether this reservation still holds every one of the seats.
	Owns(ctx context.Context, eventID string, seatIDs []string, reservationID string) (bool, error)
	// Ping reports whether holds can be taken at all, for readiness.
	Ping(ctx context.Context) error
}

// SeatRepository is the seat-catalog port.
//
// In P3 it is a catalog, not a lock. Holding a seat happens in HoldStore; what
// is left here is what a seat costs, whether it has been sold, and the
// available -> booked transition that a confirmed booking is made of.
type SeatRepository interface {
	// GetSeats returns the requested seats' catalog rows, in sorted seat-ID
	// order. It takes no locks.
	GetSeats(ctx context.Context, eventID string, seatIDs []string) ([]domain.Seat, error)
	// ListSeatsByEvent returns the seat map for an event. Seats held in the
	// hold store still read as available: that state does not live here.
	ListSeatsByEvent(ctx context.Context, eventID string) ([]domain.Seat, error)

	// LockSeatsByReservation locks the seats a reservation claims, in sorted
	// seat-ID order (D5). Only meaningful inside a transaction.
	LockSeatsByReservation(ctx context.Context, reservationID string) ([]domain.Seat, error)
	// MarkSeatsBooked flips a reservation's available seats to booked and
	// stamps its claims confirmed.
	MarkSeatsBooked(ctx context.Context, reservationID string) (int64, error)
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
	// ReleaseClaims settles an unconfirmed reservation's claim rows. It touches
	// no seat: an unconfirmed claim never owned one.
	ReleaseClaims(ctx context.Context, reservationID string) (int64, error)
	// MarkPaymentPending records that a charge may exist for this reservation
	// whose outcome nobody knows, handing it to the reconciliation job (D8). It
	// reports whether the reservation was still pending: false means it has
	// settled, and no charge may be started for it.
	MarkPaymentPending(ctx context.Context, id string) (bool, error)
	// ListReservationsAwaitingReconciliation returns pending reservations whose
	// payment outcome has been unknown since before olderThan.
	ListReservationsAwaitingReconciliation(ctx context.Context, olderThan time.Time, limit int) ([]domain.Reservation, error)
	// QueueChargeRecheck queues a reservation released on "no charge" evidence
	// for one more look at its charge key. Must run in the release's
	// transaction.
	QueueChargeRecheck(ctx context.Context, reservationID string) error
	// ListReservationsAwaitingChargeRecheck returns queued reservations
	// released before olderThan.
	ListReservationsAwaitingChargeRecheck(ctx context.Context, olderThan time.Time, limit int) ([]domain.Reservation, error)
	// ClearChargeRecheck removes a reservation from the recheck queue.
	ClearChargeRecheck(ctx context.Context, reservationID string) error
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
