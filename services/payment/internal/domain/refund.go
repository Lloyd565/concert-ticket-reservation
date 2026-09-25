package domain

import "time"

// RefundStatus is the lifecycle of one attempt to give money back. It mirrors
// Charge deliberately: a refund is a payment in the other direction and has the
// same unknown-outcome problem.
type RefundStatus string

const (
	RefundPending   RefundStatus = "pending"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
)

var refundTransitions = map[RefundStatus]map[RefundStatus]bool{
	RefundPending: {RefundSucceeded: true, RefundFailed: true},
}

// CanTransition reports whether the state machine connects from to to.
func (s RefundStatus) CanTransition(to RefundStatus) bool { return refundTransitions[s][to] }

// Terminal reports whether the outcome is settled and will not change.
func (s RefundStatus) Terminal() bool { return s != RefundPending }

// Refund reverses a succeeded charge.
type Refund struct {
	ID             string
	ChargeID       string
	AmountCents    int64
	Status         RefundStatus
	ProviderRef    string
	IdempotencyKey string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Succeed settles the refund against the provider's reference.
func (r *Refund) Succeed(providerRef string) error {
	if providerRef == "" {
		return ErrMissingProviderRef
	}
	if err := r.transition(RefundSucceeded); err != nil {
		return err
	}
	r.ProviderRef = providerRef
	return nil
}

// Fail settles the refund as refused by the provider.
func (r *Refund) Fail() error { return r.transition(RefundFailed) }

func (r *Refund) transition(to RefundStatus) error {
	if !r.Status.CanTransition(to) {
		return TransitionError{Entity: "refund", From: string(r.Status), To: string(to)}
	}
	r.Status = to
	return nil
}
