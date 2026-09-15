package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/domain"
)

// Refunder gives money back against a previous charge.
//
// It is deliberately the same shape as Charger, because a refund has the same
// two problems a charge has: it must not happen twice (FR-4.6), and its outcome
// can be unknown. The row is written pending before the provider is called for
// the same reason, and an unknown outcome leaves it pending for the same reason.
type Refunder struct {
	charges         ChargeRepository
	refunds         RefundRepository
	provider        Provider
	providerTimeout time.Duration
	log             *slog.Logger
}

// NewRefunder wires a Refunder. A zero providerTimeout falls back to
// DefaultProviderTimeout.
func NewRefunder(charges ChargeRepository, refunds RefundRepository, provider Provider, providerTimeout time.Duration, log *slog.Logger) *Refunder {
	if providerTimeout <= 0 {
		providerTimeout = DefaultProviderTimeout
	}
	return &Refunder{charges: charges, refunds: refunds, provider: provider, providerTimeout: providerTimeout, log: log}
}

// RefundInput is one request to give money back.
type RefundInput struct {
	ChargeID       string
	AmountCents    int64
	IdempotencyKey string
}

// Refund reverses a succeeded charge, at most once per idempotency key.
//
// It refuses a charge that never took money. Refunding a declined or pending
// charge would either be a no-op the caller mistakes for a real refund, or -
// worse, on a pending one - a payout against money that has not arrived.
func (r *Refunder) Refund(ctx context.Context, in RefundInput) (domain.Refund, error) {
	if in.IdempotencyKey == "" {
		return domain.Refund{}, fmt.Errorf("refund: idempotency key required: %w", domain.ErrInvalidInput)
	}
	if in.AmountCents <= 0 {
		return domain.Refund{}, fmt.Errorf("refund: amount must be positive, got %d: %w", in.AmountCents, domain.ErrInvalidInput)
	}
	if _, err := uuid.Parse(in.ChargeID); err != nil {
		return domain.Refund{}, fmt.Errorf("refund: charge id %q: %w", in.ChargeID, domain.ErrInvalidInput)
	}

	// Replay first (FR-4.6), before anything is looked up or written.
	switch existing, err := r.refunds.FindRefundByIdempotencyKey(ctx, in.IdempotencyKey); {
	case err == nil:
		return r.resume(ctx, existing, in.AmountCents)
	case !errors.Is(err, domain.ErrRefundNotFound):
		return domain.Refund{}, err
	}

	charge, err := r.charges.FindChargeByID(ctx, in.ChargeID)
	if err != nil {
		return domain.Refund{}, err
	}
	if !charge.Refundable() {
		return domain.Refund{}, fmt.Errorf("charge %s is %s: %w", charge.ID, charge.Status, domain.ErrChargeNotRefundable)
	}
	if in.AmountCents > charge.AmountCents {
		return domain.Refund{}, fmt.Errorf("charge %s took %d cents, asked to refund %d: %w",
			charge.ID, charge.AmountCents, in.AmountCents, domain.ErrRefundExceedsCharge)
	}

	now := time.Now().UTC()
	rf := domain.Refund{
		ID:             uuid.Must(uuid.NewV7()).String(),
		ChargeID:       charge.ID,
		AmountCents:    in.AmountCents,
		Status:         domain.RefundPending,
		IdempotencyKey: in.IdempotencyKey,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	// Durable before the external call, exactly as for a charge (FR-4.5).
	err = r.refunds.CreateRefund(ctx, rf)
	if errors.Is(err, domain.ErrRefundExists) {
		existing, findErr := r.refunds.FindRefundByIdempotencyKey(ctx, in.IdempotencyKey)
		if findErr != nil {
			return domain.Refund{}, fmt.Errorf("resolve concurrent refund for key %s: %w", in.IdempotencyKey, findErr)
		}
		return r.resume(ctx, existing, in.AmountCents)
	}
	if err != nil {
		return domain.Refund{}, err
	}

	return r.callProvider(ctx, rf, charge.ProviderRef)
}

// resume returns a settled refund unchanged, or re-drives an unsettled one.
func (r *Refunder) resume(ctx context.Context, existing domain.Refund, amountCents int64) (domain.Refund, error) {
	if existing.AmountCents != amountCents {
		return domain.Refund{}, fmt.Errorf("key %s refunded %d cents, now asked for %d: %w",
			existing.IdempotencyKey, existing.AmountCents, amountCents, domain.ErrAmountMismatch)
	}
	if existing.Status.Terminal() {
		return existing, nil
	}
	charge, err := r.charges.FindChargeByID(ctx, existing.ChargeID)
	if err != nil {
		return domain.Refund{}, err
	}
	r.log.InfoContext(ctx, "resuming an unsettled refund", "refund_id", existing.ID, "idempotency_key", existing.IdempotencyKey)
	return r.callProvider(ctx, existing, charge.ProviderRef)
}

// callProvider drives a pending refund to a settled state, or leaves it pending.
func (r *Refunder) callProvider(ctx context.Context, rf domain.Refund, chargeRef string) (domain.Refund, error) {
	callCtx, cancel := context.WithTimeout(ctx, r.providerTimeout)
	defer cancel()

	res, err := r.provider.Refund(callCtx, rf.IdempotencyKey, chargeRef, rf.AmountCents)
	if err != nil {
		// Unknown, not failed. Same rule as a charge: leave the row pending so
		// a retry under this key can resolve it.
		r.log.WarnContext(ctx, "refund outcome unknown; left pending for recovery",
			"refund_id", rf.ID, "idempotency_key", rf.IdempotencyKey, "error", err)
		return rf, fmt.Errorf("refund %s: %w", rf.ID, err)
	}

	settled := rf
	switch res.Outcome {
	case domain.ProviderApproved:
		if err := settled.Succeed(res.Ref); err != nil {
			return domain.Refund{}, fmt.Errorf("settle refund %s: %w", rf.ID, err)
		}
	default:
		if err := settled.Fail(); err != nil {
			return domain.Refund{}, fmt.Errorf("settle refund %s: %w", rf.ID, err)
		}
	}
	settled.UpdatedAt = time.Now().UTC()

	// Recorded even if the caller has stopped waiting, for the same reason as a
	// charge (see settleTimeout).
	settleCtx, cancelSettle := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancelSettle()

	ok, err := r.refunds.SettleRefund(settleCtx, settled)
	if err != nil {
		return domain.Refund{}, err
	}
	if !ok {
		winner, err := r.refunds.FindRefundByIdempotencyKey(settleCtx, rf.IdempotencyKey)
		if err != nil {
			return domain.Refund{}, fmt.Errorf("reread settled refund %s: %w", rf.ID, err)
		}
		return winner, nil
	}
	return settled, nil
}
