// Package domain holds entities, value objects, state machines and domain
// errors. It imports the standard library only (AGENTS.md §4): no drivers, no
// transport, no repository, and no payment provider SDK. IDs are plain strings
// so the layer stays free of any external type.
package domain

import "time"

// ChargeStatus is the lifecycle of one attempt to take money.
type ChargeStatus string

const (
	// ChargePending means a charge was started and its outcome is not known.
	// It is written before the provider is called (FR-4.5), which is what makes
	// an interrupted charge recoverable: the row proves the attempt happened.
	// A caller that reads pending has an UNKNOWN result, never a failed one -
	// the difference is the whole of D8.
	ChargePending   ChargeStatus = "pending"
	ChargeSucceeded ChargeStatus = "succeeded"
	// ChargeDeclined is the provider saying no. Definite, and not retryable.
	ChargeDeclined ChargeStatus = "declined"
	// ChargeFailed is a definite non-answer: the provider rejected the request
	// itself (bad amount, unknown instrument) rather than declining the card.
	// Also terminal. A timeout is NOT this - a timeout stays pending.
	ChargeFailed ChargeStatus = "failed"
)

// chargeTransitions is the charge state machine. Every terminal state is
// reached from pending and never left: money that moved cannot un-move, and a
// declined charge is not retried in place - a retry is a new attempt under the
// same idempotency key, which returns this same row.
var chargeTransitions = map[ChargeStatus]map[ChargeStatus]bool{
	ChargePending: {ChargeSucceeded: true, ChargeDeclined: true, ChargeFailed: true},
}

// CanTransition reports whether the state machine connects from to to.
func (s ChargeStatus) CanTransition(to ChargeStatus) bool { return chargeTransitions[s][to] }

// Terminal reports whether the outcome is settled and will not change.
func (s ChargeStatus) Terminal() bool { return s != ChargePending }

// Charge is one attempt to take money for one reservation.
type Charge struct {
	ID             string
	ReservationID  string
	UserID         string
	AmountCents    int64
	Status         ChargeStatus
	ProviderRef    string
	DeclineReason  string
	IdempotencyKey string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Succeed settles the charge against the provider's reference. The reference is
// required: a succeeded charge with nothing to refund against is unrefundable,
// and the database rejects it anyway.
func (c *Charge) Succeed(providerRef string) error {
	if providerRef == "" {
		return ErrMissingProviderRef
	}
	if err := c.transition(ChargeSucceeded); err != nil {
		return err
	}
	c.ProviderRef = providerRef
	return nil
}

// Decline settles the charge as refused by the provider.
func (c *Charge) Decline(reason string) error {
	if err := c.transition(ChargeDeclined); err != nil {
		return err
	}
	c.DeclineReason = reason
	return nil
}

// Fail settles the charge as rejected before any money could move.
func (c *Charge) Fail(reason string) error {
	if err := c.transition(ChargeFailed); err != nil {
		return err
	}
	c.DeclineReason = reason
	return nil
}

// Refundable reports whether money actually moved and can be given back.
func (c *Charge) Refundable() bool { return c.Status == ChargeSucceeded }

func (c *Charge) transition(to ChargeStatus) error {
	if !c.Status.CanTransition(to) {
		return TransitionError{Entity: "charge", From: string(c.Status), To: string(to)}
	}
	c.Status = to
	return nil
}
