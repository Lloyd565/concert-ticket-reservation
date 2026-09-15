package usecase

import (
	"context"
	"log/slog"
	"time"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
)

const (
	// expireInterval is how often the expirer looks. Nothing waits on it but an
	// email: the seats went back on sale the moment their Redis TTL ran out.
	expireInterval = 15 * time.Second
	// expireBatch bounds one pass so a backlog cannot hold a long transaction.
	expireBatch = 100
)

// Expirer settles reservations whose checkout window closed unpaid, and records
// reservation.expired in the same transaction (D9).
//
// It exists because an event needs a state change to be written with. Since P3 a
// lapsed reservation was left pending with a deadline in the past, which is
// correct for seats but gives the outbox nothing to commit alongside - and an
// event emitted without a transaction to join is the lost event D9 forbids.
//
// It is not the P0 sweeper under a new name, which is what AGENTS.md warns
// against. It releases no seat and touches no Redis key: expiry of the hold is
// still the TTL, unaided (§6.2 row 3). It moves one status column, on a row
// nothing else will move again, and every replica can run it because each row is
// claimed by lock and guarded by its status. Reservations whose payment outcome
// is unknown are never expired here - they belong to reconciliation (D8).
type Expirer struct {
	tx           TxManager
	reservations ReservationRepository
	outbox       Outbox
	log          *slog.Logger
}

// NewExpirer wires an Expirer.
func NewExpirer(tx TxManager, reservations ReservationRepository, outbox Outbox, log *slog.Logger) *Expirer {
	return &Expirer{tx: tx, reservations: reservations, outbox: outbox, log: log}
}

// Run expires lapsed reservations until ctx is cancelled.
func (e *Expirer) Run(ctx context.Context) {
	ticker := time.NewTicker(expireInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := e.ExpireOnce(ctx); err != nil {
				// Nothing is lost by a failed pass: the reservations are still
				// pending and still lapsed, and the next tick finds them again.
				e.log.ErrorContext(ctx, "expiry pass failed", "error", err)
			}
		}
	}
}

// ExpireOnce runs a single pass and returns how many reservations it expired.
// Exported so tests can drive it without waiting on the ticker.
func (e *Expirer) ExpireOnce(ctx context.Context) (int, error) {
	var expired int
	err := e.tx.WithinTx(ctx, func(ctx context.Context) error {
		lapsed, err := e.reservations.ExpireLapsedReservations(ctx, expireBatch)
		if err != nil {
			return err
		}
		for _, r := range lapsed {
			if err := e.outbox.Enqueue(ctx, domain.EventReservationExpired, r.ID, domain.ReservationExpiredEvent{
				ReservationID: r.ID,
				UserID:        r.UserID,
				EventID:       r.EventID,
				ExpiresAt:     r.ExpiresAt,
			}); err != nil {
				return err
			}
		}
		expired = len(lapsed)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if expired > 0 {
		e.log.InfoContext(ctx, "lapsed reservations expired", "expired", expired)
	}
	return expired, nil
}
