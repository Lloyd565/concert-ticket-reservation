package domain

import (
	"errors"
	"fmt"
)

// Sentinel errors. Transport maps these to gRPC codes; SQL errors and provider
// SDK errors are never allowed to reach a caller (AGENTS.md §5).
var (
	// ErrChargeNotFound means no charge matched the lookup - normal on the
	// first request for an idempotency key, and the answer reconciliation gets
	// when a reservation was never actually charged.
	ErrChargeNotFound = errors.New("charge not found")

	// ErrRefundNotFound means no refund matched the lookup.
	ErrRefundNotFound = errors.New("refund not found")

	// ErrChargeExists means the idempotency key is already taken. Raised by the
	// UNIQUE index, not by an application check: two concurrent submits of the
	// same key both pass a check-then-act test, and only the index stops them.
	ErrChargeExists = errors.New("charge already exists for this idempotency key")

	// ErrRefundExists is ErrChargeExists for refunds.
	ErrRefundExists = errors.New("refund already exists for this idempotency key")

	// ErrChargeNotRefundable means the charge never took money, so there is
	// nothing to give back.
	ErrChargeNotRefundable = errors.New("charge is not refundable")

	// ErrRefundExceedsCharge guards against refunding more than was taken.
	ErrRefundExceedsCharge = errors.New("refund exceeds the original charge")

	// ErrAmountMismatch means an idempotency key was replayed with a different
	// amount. Returning the original charge would be wrong (the caller asked
	// for something else) and charging again would be worse, so it is an error.
	ErrAmountMismatch = errors.New("idempotency key replayed with a different amount")

	// ErrInvalidTransition is the sentinel that every TransitionError matches.
	ErrInvalidTransition = errors.New("invalid state transition")

	// ErrMissingProviderRef guards settling a charge or refund with no
	// provider reference to refund or audit against.
	ErrMissingProviderRef = errors.New("provider reference required to settle")

	// ErrInvalidInput marks a caller mistake - a malformed ID, a missing
	// required field - so the boundary can answer InvalidArgument, not Internal.
	ErrInvalidInput = errors.New("invalid input")

	// ErrProviderUnavailable means the provider call did not complete: a
	// timeout, a dropped connection, a tripped breaker.
	//
	// This is the single most consequential error in the service. It does NOT
	// mean the charge failed. The money may have moved. Every caller must treat
	// it as unknown and resolve it by idempotency key, never by assuming
	// failure (D8).
	ErrProviderUnavailable = errors.New("payment provider unavailable: outcome unknown")
)

// TransitionError reports an attempt to move an entity between two states the
// state machine does not connect. Returned rather than ignored: a silent no-op
// here would let a caller believe money moved when it did not.
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
