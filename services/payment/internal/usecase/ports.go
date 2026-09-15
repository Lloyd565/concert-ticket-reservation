// Package usecase holds application services: orchestration and the decisions
// about what must be durable before what. It depends on domain and on the
// interfaces declared below, never on their implementations, on a database
// driver, or on a payment provider SDK (AGENTS.md §4).
package usecase

import (
	"context"

	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/domain"
)

// ChargeRepository is the charge-state port.
//
// There is no TxManager in this service. Every write below is a single
// statement, and single statements are already atomic; Booking needs
// transaction boundaries because it holds locks across a check and an act,
// Payment does not.
type ChargeRepository interface {
	// CreateCharge inserts a pending charge. It returns
	// domain.ErrChargeExists if the idempotency key is already taken, which is
	// how the UNIQUE index turns a concurrent double-submit into one charge.
	CreateCharge(ctx context.Context, c domain.Charge) error
	// FindChargeByIdempotencyKey returns domain.ErrChargeNotFound when the key
	// has not been seen before.
	FindChargeByIdempotencyKey(ctx context.Context, key string) (domain.Charge, error)
	// FindChargeByID returns domain.ErrChargeNotFound when the id is unknown.
	FindChargeByID(ctx context.Context, id string) (domain.Charge, error)
	// SettleCharge records a terminal outcome. It only affects a row that is
	// still pending, so a late-arriving second result cannot overwrite a
	// settled one, and reports whether it did.
	//
	// The event announcing the outcome is recorded by the same statement (D9),
	// and only if this call was the one that settled the charge.
	SettleCharge(ctx context.Context, c domain.Charge, eventType string, payload any) (bool, error)
}

// RefundRepository is the refund port. Shaped exactly like ChargeRepository
// because a refund has the same durability problem in the other direction.
type RefundRepository interface {
	CreateRefund(ctx context.Context, r domain.Refund) error
	FindRefundByIdempotencyKey(ctx context.Context, key string) (domain.Refund, error)
	// SettleRefund is SettleCharge for refunds. An empty eventType records no
	// event.
	SettleRefund(ctx context.Context, r domain.Refund, eventType string, payload any) (bool, error)
}

// Provider is the payment provider port (ARCHITECTURE.md §7: a mock adapter
// behind an interface, because real money is out of scope but the integration
// pattern is not).
//
// Implementations return domain.ErrProviderUnavailable - and nothing else - for
// a call that did not complete. That is the contract the saga's D8 handling
// rests on: an error from here means unknown, not failed.
type Provider interface {
	// Charge takes money. idempotencyKey is passed through to the provider, so
	// a retry of a call that already reached it returns the original result
	// rather than taking the money twice (AGENTS.md §10).
	Charge(ctx context.Context, idempotencyKey string, amountCents int64) (domain.ProviderResult, error)
	// Refund gives money back against a previous charge's provider reference.
	Refund(ctx context.Context, idempotencyKey, chargeRef string, amountCents int64) (domain.ProviderResult, error)
}
