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
)

// ErrBackstopTripped means the database-level backstop (D13, the partial unique
// index on reservation_seats) refused a second active claim on a seat.
//
// The outcome is still correct for the caller - it wraps ErrSeatUnavailable, so
// the boundary answers 409 - but reaching it means the application-level
// SELECT ... FOR UPDATE path failed to do its job. Tests assert this never
// happens; production should alert on it.
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
