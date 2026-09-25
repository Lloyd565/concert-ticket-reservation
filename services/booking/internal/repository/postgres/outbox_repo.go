package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/logging"
)

// The outbox half of the repository (P4, D9). Enqueue is what the use cases
// write through; ClaimOutbox and MarkOutboxPublished are what the relay drains
// through.

// Enqueue writes an event to the outbox inside the caller's transaction.
//
// It refuses to run outside one, and that refusal is the reason it checks for
// the transaction itself instead of falling back to the pool the way q does. An
// event inserted on its own connection commits whether or not the state change
// beside it does: the reservation rolls back and its event is published anyway,
// or the booking commits and its event is lost to a crash - the two failures
// the outbox exists to rule out.
func (r *Repo) Enqueue(ctx context.Context, eventType, aggregateID string, payload any) error {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	if !ok {
		return fmt.Errorf("enqueue %s: an outbox write must join the transaction of the state change it describes", eventType)
	}
	aggID, err := uuid.Parse(aggregateID)
	if err != nil {
		return fmt.Errorf("parse aggregate id %q: %w", aggregateID, domain.ErrInvalidInput)
	}

	id := uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	// The envelope is built now, not when the relay gets to it: a publish that
	// is delayed by a broker outage still says what happened at the time.
	body, err := json.Marshal(domain.Envelope{
		EventID:       id.String(),
		EventType:     eventType,
		OccurredAt:    now,
		CorrelationID: logging.CorrelationID(ctx),
		Version:       domain.EventVersion,
		Payload:       payload,
	})
	if err != nil {
		return fmt.Errorf("encode %s: %w", eventType, err)
	}
	if err := r.queries.WithTx(tx).InsertOutboxEvent(ctx, InsertOutboxEventParams{
		ID:          id,
		AggregateID: aggID,
		EventType:   eventType,
		Payload:     body,
		CreatedAt:   now,
	}); err != nil {
		return fmt.Errorf("insert %s into outbox: %w", eventType, err)
	}
	return nil
}

// ClaimOutbox leases up to limit unpublished events to the caller, oldest
// first. The lease commits before this returns: the relay publishes with nothing
// locked (D4).
func (r *Repo) ClaimOutbox(ctx context.Context, limit int, lease time.Duration) ([]domain.OutboxMessage, error) {
	rows, err := r.q(ctx).ClaimOutboxBatch(ctx, ClaimOutboxBatchParams{
		BatchSize:    int32(limit), //nolint:gosec // limit is a small constant
		LeaseSeconds: lease.Seconds(),
	})
	if err != nil {
		return nil, fmt.Errorf("claim outbox batch: %w", err)
	}
	out := make([]domain.OutboxMessage, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.OutboxMessage{
			ID:        row.ID.String(),
			EventType: row.EventType,
			Payload:   row.Payload,
			CreatedAt: row.CreatedAt,
		})
	}
	// UPDATE ... RETURNING has no order. Publishing oldest first keeps one
	// reservation's events in the order they happened, within a batch.
	slices.SortFunc(out, func(a, b domain.OutboxMessage) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

// MarkOutboxPublished records that the broker confirmed these events.
func (r *Repo) MarkOutboxPublished(ctx context.Context, ids []string) error {
	parsed := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		u, err := uuid.Parse(id)
		if err != nil {
			return fmt.Errorf("parse outbox id %q: %w", id, domain.ErrInvalidInput)
		}
		parsed = append(parsed, u)
	}
	if err := r.q(ctx).MarkOutboxPublished(ctx, parsed); err != nil {
		return fmt.Errorf("mark outbox published: %w", err)
	}
	return nil
}

// ExpireLapsedReservations moves lapsed, undisputed pending reservations to
// expired and returns them.
func (r *Repo) ExpireLapsedReservations(ctx context.Context, limit int) ([]domain.LapsedReservation, error) {
	rows, err := r.q(ctx).ExpireLapsedReservations(ctx, int32(limit)) //nolint:gosec // limit is a small constant
	if err != nil {
		return nil, fmt.Errorf("expire lapsed reservations: %w", err)
	}
	out := make([]domain.LapsedReservation, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.LapsedReservation{
			ID:        row.ID.String(),
			UserID:    row.UserID.String(),
			EventID:   row.EventID.String(),
			ExpiresAt: row.ExpiresAt,
		})
	}
	return out, nil
}
