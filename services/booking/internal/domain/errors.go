package domain

import (
	"errors"
	"fmt"
)

// Sentinel errors. Transport maps these to status codes; SQL errors are never
// allowed to reach a client (AGENTS.md §5).
var (
	// ErrSeatUnavailable is returned when one or more requested seats are not
	// in the available state. It is deliberately distinguishable from a
	// generic failure so callers can return 409 rather than 500 (FR-3.2).
	ErrSeatUnavailable = errors.New("seat unavailable")

	// ErrSeatNotFound means a requested seat ID does not exist for the event.
	ErrSeatNotFound = errors.New("seat not found")

	// ErrInvalidTransition is the sentinel that every TransitionError matches.
	ErrInvalidTransition = errors.New("invalid state transition")

	// ErrNoSeatsRequested guards the degenerate hold request.
	ErrNoSeatsRequested = errors.New("no seats requested")

	// ErrInvalidInput marks a caller mistake - a malformed ID, a missing
	// required field - so the boundary can answer 400 instead of 500.
	ErrInvalidInput = errors.New("invalid input")

	// ErrHoldsUnavailable is D12 in error form: the hold store cannot be
	// reached, so no seat can be claimed safely.
	//
	// It is deliberately not ErrSeatUnavailable. The seats may be perfectly
	// free; what is missing is the only mechanism that can hand exactly one of
	// them to exactly one caller. The correct answer is 503 and a retry, never
	// a fallback to an unlocked path - a hold taken without the lock is a
	// double-booking waiting for a second caller (D12, AGENTS.md §2 rule 11).
	ErrHoldsUnavailable = errors.New("hold store unavailable; refusing to claim seats without a lock")
)

// ErrBackstopTripped means the database-level backstop (D13, the partial unique
// index on reservation_seats) refused a second confirmed claim on a seat.
//
// The outcome is still correct for the caller - it wraps ErrSeatUnavailable, so
// the boundary answers 409 - but reaching it means the Redis claim path and the
// SELECT ... FOR UPDATE in confirmation both failed to do their job. Tests
// assert this never happens; production should alert on it.
var ErrBackstopTripped = fmt.Errorf("claim backstop tripped: %w", ErrSeatUnavailable)

// TransitionError reports an attempt to move an entity between two states that
// the state machine does not connect. It is returned rather than silently
// ignored: a no-op here would let a caller believe a seat changed state when it
// did not, which is exactly how a double-booking gets rationalised.
type TransitionError struct {
	Entity string
	From   string
	To     string
}

func (e TransitionError) Error() string {
	return fmt.Sprintf("%s: illegal transition %s -> %s", e.Entity, e.From, e.To)
}

// Is lets errors.Is(err, ErrInvalidTransition) match any TransitionError.
func (e TransitionError) Is(target error) bool { return target == ErrInvalidTransition }

// ErrReservationNotFound means no reservation matched the lookup - normal on
// the first request for an idempotency key.
var ErrReservationNotFound = errors.New("reservation not found")

// ErrDuplicateRequest means an identical idempotency key is being processed
// concurrently. The retry is safe to repeat once the first one settles.
var ErrDuplicateRequest = errors.New("duplicate request in flight")

// Saga errors (P2).
var (
	// ErrNotReservationOwner means the caller is not the user who holds the
	// reservation. A reservation ID is not a capability: knowing one must not
	// be enough to pay for, confirm or cancel somebody else's seats.
	ErrNotReservationOwner = errors.New("reservation belongs to another user")

	// ErrReservationNotPayable means the reservation is not in a state a
	// payment can be made against - already confirmed, failed, or expired
	// (FR-4.2).
	ErrReservationNotPayable = errors.New("reservation is not payable")

	// ErrPaymentDeclined means the provider definitively refused. The seats
	// have been released and the reservation failed by the time a caller sees
	// this, so it is a settled outcome, not a retryable one.
	ErrPaymentDeclined = errors.New("payment declined")

	// ErrPaymentOutcomeUnknown is the D8 error, and the most important one in
	// this package.
	//
	// It means the payment call did not complete: it timed out, the connection
	// dropped, or the circuit breaker was open. It does NOT mean the payment
	// failed. The money may have moved. Nothing may release the seats on this
	// error - the reservation stays pending, marked for reconciliation, and the
	// reconciliation job resolves it against Payment by idempotency key.
	//
	// Every time this sentinel is handled, the correct action is to do nothing
	// to seat state. If a code path handling it releases a seat, that path is
	// wrong regardless of what else it does.
	ErrPaymentOutcomeUnknown = errors.New("payment outcome unknown; left for reconciliation")

	// ErrConfirmUnrecoverable means a paid reservation can never be confirmed:
	// its checkout window closed and its seats are gone, or they are no longer
	// in a state that can become booked. It is the trigger for the automatic
	// refund that keeps FR-5.3 true - there is no such thing as a customer who
	// paid and got nothing.
	ErrConfirmUnrecoverable = errors.New("reservation can no longer be confirmed")

	// ErrBookingNotFound means no booking exists for the lookup.
	ErrBookingNotFound = errors.New("booking not found")

	// ErrBookingExists means the reservation already has a booking. Raised by
	// the bookings.reservation_id UNIQUE constraint, which is the last line of
	// defence against a retry issuing a second set of tickets.
	ErrBookingExists = errors.New("booking already exists for this reservation")
)
