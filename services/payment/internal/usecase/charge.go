package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/google/uuid"

	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/domain"
)

// Defaults for the provider call. All are overridable from config; these are
// what a caller gets if it passes zero.
const (
	// DefaultProviderTimeout is per attempt. Every attempt and the backoff
	// between them must fit inside the caller's deadline, which gRPC carries in
	// (Booking's is 3s): an attempt that uses the whole deadline leaves the retry
	// nothing to run in.
	DefaultProviderTimeout = time.Second
	DefaultMaxAttempts     = 2
	DefaultRetryBackoff    = 100 * time.Millisecond
)

// settleTimeout bounds the write that records a provider's answer.
//
// That write runs on a context detached from the caller's. Once the provider has
// answered, money has moved or definitely has not, and Booking's deadline firing
// a moment too early is no reason to forget which. Inheriting the cancellation
// would leave the row pending with the answer lost - the same state a crash
// leaves, recoverable only by re-driving the charge and trusting the provider to
// replay it.
const settleTimeout = 5 * time.Second

// Charger takes money for a reservation.
//
// A note on what this service does NOT validate. FR-4.1 and FR-4.2 require a
// charge to be checked against its reservation's amount and expiry, and to be
// rejected for an expired or already-confirmed reservation. Those checks are
// made by the saga in Booking before it calls here, because Payment cannot read
// booking_db (D2) and calling back into Booking would turn the one synchronous
// service-to-service hop in the system into a cycle. What Payment enforces is
// what Payment owns: one idempotency key means at most one movement of money,
// and a key is never reused for a different amount.
type Charger struct {
	charges         ChargeRepository
	provider        Provider
	providerTimeout time.Duration
	maxAttempts     int
	backoff         time.Duration
	log             *slog.Logger
}

// NewCharger wires a Charger. Zero values fall back to the Default constants.
func NewCharger(charges ChargeRepository, provider Provider, providerTimeout time.Duration, maxAttempts int, log *slog.Logger) *Charger {
	if providerTimeout <= 0 {
		providerTimeout = DefaultProviderTimeout
	}
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	return &Charger{
		charges:         charges,
		provider:        provider,
		providerTimeout: providerTimeout,
		maxAttempts:     maxAttempts,
		backoff:         DefaultRetryBackoff,
		log:             log,
	}
}

// ChargeInput is one request to take money.
type ChargeInput struct {
	ReservationID  string
	UserID         string
	AmountCents    int64
	IdempotencyKey string
}

// Charge takes money for a reservation, at most once per idempotency key.
//
// The ordering is the requirement (FR-4.5): the charge row is durable, in the
// pending state, BEFORE the provider is called. If the process dies during the
// call, that row is the evidence an attempt was made - and a later call under
// the same key finds it and drives it to a conclusion instead of starting a
// second, invisible charge.
//
// The error to read carefully is domain.ErrProviderUnavailable. It means the
// call did not complete, NOT that it failed: the charge stays pending and the
// money may well have moved. Callers must treat it as unknown (D8).
func (c *Charger) Charge(ctx context.Context, in ChargeInput) (domain.Charge, error) {
	if in.IdempotencyKey == "" {
		return domain.Charge{}, fmt.Errorf("charge: idempotency key required: %w", domain.ErrInvalidInput)
	}
	if in.AmountCents <= 0 {
		return domain.Charge{}, fmt.Errorf("charge: amount must be positive, got %d: %w", in.AmountCents, domain.ErrInvalidInput)
	}
	if _, err := uuid.Parse(in.ReservationID); err != nil {
		return domain.Charge{}, fmt.Errorf("charge: reservation id %q: %w", in.ReservationID, domain.ErrInvalidInput)
	}
	if _, err := uuid.Parse(in.UserID); err != nil {
		return domain.Charge{}, fmt.Errorf("charge: user id %q: %w", in.UserID, domain.ErrInvalidInput)
	}

	// Replay first (FR-4.3). A key that has been seen before never produces a
	// second charge, whatever state the first one is in.
	switch existing, err := c.charges.FindChargeByIdempotencyKey(ctx, in.IdempotencyKey); {
	case err == nil:
		return c.resume(ctx, existing, in.AmountCents)
	case !errors.Is(err, domain.ErrChargeNotFound):
		return domain.Charge{}, err
	}

	now := time.Now().UTC()
	ch := domain.Charge{
		ID:             uuid.Must(uuid.NewV7()).String(),
		ReservationID:  in.ReservationID,
		UserID:         in.UserID,
		AmountCents:    in.AmountCents,
		Status:         domain.ChargePending,
		IdempotencyKey: in.IdempotencyKey,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	// FR-4.5. Nothing external has been called yet, and nothing external is
	// called until this row is committed.
	err := c.charges.CreateCharge(ctx, ch)
	if errors.Is(err, domain.ErrChargeExists) {
		// Two submits of the same key raced past the read above. The UNIQUE
		// index picked a winner; this caller joins that charge rather than
		// running a parallel one.
		existing, findErr := c.charges.FindChargeByIdempotencyKey(ctx, in.IdempotencyKey)
		if findErr != nil {
			return domain.Charge{}, fmt.Errorf("resolve concurrent charge for key %s: %w", in.IdempotencyKey, findErr)
		}
		return c.resume(ctx, existing, in.AmountCents)
	}
	if err != nil {
		return domain.Charge{}, err
	}

	return c.callProvider(ctx, ch)
}

// GetCharge answers: what happened to the charge under this key?
//
// This is the query that makes Booking's unknown-outcome case converge. The
// reconciliation job asks it and gets either a settled answer or pending,
// meaning ask again (ARCHITECTURE.md §6.3).
func (c *Charger) GetCharge(ctx context.Context, idempotencyKey string) (domain.Charge, error) {
	if idempotencyKey == "" {
		return domain.Charge{}, fmt.Errorf("get charge: idempotency key required: %w", domain.ErrInvalidInput)
	}
	return c.charges.FindChargeByIdempotencyKey(ctx, idempotencyKey)
}

// resume decides what to do with a charge that already exists for this key.
//
// A settled charge is returned as-is; that is the whole point of an idempotency
// key. A charge still pending is the recovery case FR-4.5 exists for: an
// earlier attempt was interrupted between writing the row and settling it, so
// the provider is called again under the same key. The provider's own
// idempotency makes that safe - it returns the original result rather than
// taking the money a second time.
func (c *Charger) resume(ctx context.Context, existing domain.Charge, amountCents int64) (domain.Charge, error) {
	if existing.AmountCents != amountCents {
		return domain.Charge{}, fmt.Errorf("key %s was charged %d cents, now asked for %d: %w",
			existing.IdempotencyKey, existing.AmountCents, amountCents, domain.ErrAmountMismatch)
	}
	if existing.Status.Terminal() {
		return existing, nil
	}
	c.log.InfoContext(ctx, "resuming an unsettled charge", "charge_id", existing.ID, "idempotency_key", existing.IdempotencyKey)
	return c.callProvider(ctx, existing)
}

// callProvider drives a pending charge to a settled state, or leaves it pending.
func (c *Charger) callProvider(ctx context.Context, ch domain.Charge) (domain.Charge, error) {
	res, err := c.attemptWithRetry(ctx, ch)
	if err != nil {
		// Unknown outcome. The row stays pending on purpose: it is the only
		// record that this attempt happened, and overwriting it with failed
		// would turn "we do not know" into "it did not happen" - exactly the
		// mistake D8 forbids one layer up.
		c.log.WarnContext(ctx, "charge outcome unknown; left pending for recovery",
			"charge_id", ch.ID, "idempotency_key", ch.IdempotencyKey, "error", err)
		return ch, fmt.Errorf("charge %s: %w", ch.ID, err)
	}

	settled := ch
	switch res.Outcome {
	case domain.ProviderApproved:
		if err := settled.Succeed(res.Ref); err != nil {
			return domain.Charge{}, fmt.Errorf("settle charge %s: %w", ch.ID, err)
		}
	default:
		if err := settled.Decline(res.Reason); err != nil {
			return domain.Charge{}, fmt.Errorf("settle charge %s: %w", ch.ID, err)
		}
	}
	settled.UpdatedAt = time.Now().UTC()

	// The provider has answered, so the answer is recorded even if the caller
	// has stopped waiting for it (see settleTimeout).
	settleCtx, cancelSettle := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancelSettle()

	// The UPDATE only touches rows that are still pending. If it changed
	// nothing, another attempt under this key settled first; that result is
	// authoritative and this one is a duplicate of it.
	ok, err := c.charges.SettleCharge(settleCtx, settled)
	if err != nil {
		return domain.Charge{}, err
	}
	if !ok {
		winner, err := c.charges.FindChargeByIdempotencyKey(settleCtx, ch.IdempotencyKey)
		if err != nil {
			return domain.Charge{}, fmt.Errorf("reread settled charge %s: %w", ch.ID, err)
		}
		return winner, nil
	}
	return settled, nil
}

// attemptWithRetry calls the provider with a deadline and bounded retries
// (FR-4.4, ARCHITECTURE.md §10).
//
// Retrying a payment is only safe because the idempotency key goes to the
// provider too: a second attempt after a timeout returns the first attempt's
// result instead of taking the money again. Without that key this loop would be
// a double-charge generator, which is why AGENTS.md §10 pairs them.
//
// A decline is not retried. It is a definite answer, and asking again only
// annoys the customer's bank.
func (c *Charger) attemptWithRetry(ctx context.Context, ch domain.Charge) (domain.ProviderResult, error) {
	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if attempt > 1 {
			// Exponential backoff with jitter: a fleet retrying in lockstep is
			// how a struggling provider is kept struggling.
			delay := c.backoff << (attempt - 2)
			jitter := time.Duration(rand.Int63n(int64(delay) + 1)) //nolint:gosec // spreading retries, not generating secrets
			select {
			case <-ctx.Done():
				return domain.ProviderResult{}, fmt.Errorf("%w: %v", domain.ErrProviderUnavailable, ctx.Err())
			case <-time.After(delay + jitter):
			}
		}
		// Each attempt gets its own deadline, so one hung call cannot eat the
		// whole budget and leave the retry no time to run.
		callCtx, cancel := context.WithTimeout(ctx, c.providerTimeout)
		res, err := c.provider.Charge(callCtx, ch.IdempotencyKey, ch.AmountCents)
		cancel()
		if err == nil {
			return res, nil
		}
		lastErr = err
	}
	return domain.ProviderResult{}, lastErr
}
