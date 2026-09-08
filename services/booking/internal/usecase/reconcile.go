package usecase

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
)

// Defaults for the reconciliation job.
const (
	// DefaultReconcileInterval is how often the job sweeps. It must be well
	// under the hold TTL: these reservations are the ones the sweeper is
	// forbidden to touch, so nothing else will ever free their seats.
	DefaultReconcileInterval = 15 * time.Second
	// DefaultReconcileGrace is how long a payment outcome may stay unknown
	// before the job intervenes. Long enough that a slow-but-succeeding call
	// finishes and answers the customer directly; short enough that a seat is
	// not stranded.
	DefaultReconcileGrace = 30 * time.Second
	// DefaultReconcileBatch bounds one pass so a backlog cannot stall it.
	DefaultReconcileBatch = 50
)

// Reconciler resolves reservations whose payment outcome is unknown
// (ARCHITECTURE.md §6.3).
//
// This job is what makes D8 a design rather than a way of leaking seats.
// LeavePendingForReconciliation deliberately does nothing on a timeout, which is
// only correct because something else eventually finds out what happened. This
// is that something. Without it, every timed-out payment strands its seats until
// a human intervenes.
//
// It asks Payment one question - what happened to the charge under this
// reservation's key? - and there are exactly four answers, each with one
// correct action:
//
//	succeeded  -> confirm; if confirmation is impossible, refund (§6.2 row 4)
//	declined   -> release the seats, reservation failed (§6.2 row 1, resolved late)
//	no charge  -> release the seats; proof that no money moved
//	pending    -> Payment holds a charge it never settled. Re-drive it under the
//	              same key, which settles it into one of the three above
type Reconciler struct {
	saga         *Saga
	reservations ReservationRepository
	payments     PaymentClient
	interval     time.Duration
	grace        time.Duration
	batch        int
	log          *slog.Logger
}

// NewReconciler wires a Reconciler. Zero values fall back to the Default
// constants.
func NewReconciler(saga *Saga, reservations ReservationRepository, payments PaymentClient, interval, grace time.Duration, batch int, log *slog.Logger) *Reconciler {
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	if grace <= 0 {
		grace = DefaultReconcileGrace
	}
	if batch <= 0 {
		batch = DefaultReconcileBatch
	}
	return &Reconciler{saga: saga, reservations: reservations, payments: payments, interval: interval, grace: grace, batch: batch, log: log}
}

// Run reconciles until ctx is cancelled. Intended to be started in its own
// goroutine at boot.
func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := r.ReconcileOnce(ctx); err != nil {
				// A failed pass is not fatal: the next tick retries, and the
				// seats stay held meanwhile rather than being wrongly freed.
				r.log.ErrorContext(ctx, "reconciliation pass failed", "error", err)
			}
		}
	}
}

// ReconcileOnce runs a single pass and returns how many reservations it
// resolved. Exported so tests can drive it deterministically instead of waiting
// on the ticker.
func (r *Reconciler) ReconcileOnce(ctx context.Context) (int, error) {
	stuck, err := r.reservations.ListReservationsAwaitingReconciliation(ctx, time.Now().UTC().Add(-r.grace), r.batch)
	if err != nil {
		return 0, err
	}

	resolved := 0
	for _, res := range stuck {
		if err := r.resolve(ctx, res); err != nil {
			// One reservation failing must not abandon the rest of the batch:
			// the common reason is that Payment is still unreachable, which is
			// true for all of them and self-correcting for none of them.
			r.log.WarnContext(ctx, "could not resolve reservation; will retry",
				"reservation_id", res.ID, "unknown_since", res.PaymentPendingSince, "error", err)
			continue
		}
		resolved++
	}
	if resolved > 0 {
		r.log.InfoContext(ctx, "reconciliation resolved reservations", "resolved", resolved, "examined", len(stuck))
	}
	return resolved, nil
}

// resolve asks Payment what happened and applies the one action that answer
// justifies.
func (r *Reconciler) resolve(ctx context.Context, res domain.Reservation) error {
	result, err := r.payments.GetCharge(ctx, ChargeIdempotencyKey(res.ID))
	if err != nil {
		// Still cannot ask. The reservation keeps its seats and its mark; the
		// next pass tries again. This is the loop converging slowly, not
		// failing - and slow convergence is the price D8 charges for never
		// releasing a seat somebody may have paid for.
		return err
	}

	if result.Status == domain.PaymentPending {
		// Payment holds a charge it never settled: its own provider call was
		// interrupted, so the row is stuck in the state FR-4.5 puts it in
		// before calling out.
		//
		// Asking again would wait forever, because nothing else is going to
		// settle it. Re-driving the charge under the same key is what does -
		// Payment resumes the existing charge rather than creating a second
		// one, and passes the same key to the provider, so no money moves
		// twice. This is the step that turns "leave it pending" from a leak
		// into convergence.
		r.log.InfoContext(ctx, "re-driving an unsettled charge",
			"reservation_id", res.ID, "charge_id", result.ChargeID)
		result, err = r.payments.Charge(ctx, ChargeRequest{
			ReservationID:  res.ID,
			UserID:         res.UserID,
			AmountCents:    res.TotalCents,
			IdempotencyKey: ChargeIdempotencyKey(res.ID),
		})
		if err != nil {
			// Still no answer. Nothing changes; the next pass tries again.
			return err
		}
	}

	switch result.Status {
	case domain.PaymentSucceeded:
		// The money moved. Deliver the seats if that is still possible, and
		// give the money back if it is not (§6.2 row 4, FR-5.3).
		_, err := r.saga.ConfirmPaid(ctx, res.ID, result)
		if errors.Is(err, domain.ErrConfirmUnrecoverable) {
			r.log.WarnContext(ctx, "paid reservation cannot be confirmed; refunding",
				"reservation_id", res.ID, "charge_id", result.ChargeID, "reason", err)
			return r.saga.RefundUnconfirmable(ctx, res, result)
		}
		return err

	case domain.PaymentDeclined, domain.PaymentFailed:
		// §6.2 row 1, reached late. The answer was there all along; only the
		// original call failed to bring it back.
		return r.saga.ReleaseDeclined(ctx, res.ID, result.DeclineReason)

	case domain.PaymentNoCharge:
		// Payment has never seen this key, so the original call never reached
		// it. No money moved, and releasing is safe - the only circumstance in
		// which that is true after a timeout.
		return r.saga.ReleaseNeverCharged(ctx, res.ID)

	default:
		// Unreachable in practice: Payment answers a charge with a settled
		// result or an error, never with a pending value. Kept as the safe
		// default, because the safe thing to do with an outcome this code does
		// not recognise is nothing at all.
		r.log.WarnContext(ctx, "charge still unsettled after a re-drive; leaving reservation pending",
			"reservation_id", res.ID, "charge_id", result.ChargeID, "status", result.Status)
		return nil
	}
}
