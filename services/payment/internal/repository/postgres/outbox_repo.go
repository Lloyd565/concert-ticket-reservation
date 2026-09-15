package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/logging"
)

// The outbox half of the repository (P4, D9). Payment has no transactions to
// join, so events are not enqueued separately: SettleCharge and SettleRefund
// write them in the settling statement. What is here is the relay's side.

// encodeEvent builds the envelope for an outbox row. An empty eventType means
// the write announces nothing, and yields no ID and no body.
func encodeEvent(ctx context.Context, eventType string, occurredAt time.Time, payload any) (uuid.UUID, []byte, error) {
	if eventType == "" {
		return uuid.Nil, nil, nil
	}
	id := uuid.Must(uuid.NewV7())
	body, err := json.Marshal(domain.Envelope{
		EventID:       id.String(),
		EventType:     eventType,
		OccurredAt:    occurredAt.UTC(),
		CorrelationID: logging.CorrelationID(ctx),
		Version:       domain.EventVersion,
		Payload:       payload,
	})
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("encode %s: %w", eventType, err)
	}
	return id, body, nil
}

// ClaimOutbox leases up to limit unpublished events to the caller, oldest
// first. The lease commits before this returns: the relay publishes with nothing
// locked (D4).
func (r *Repo) ClaimOutbox(ctx context.Context, limit int, lease time.Duration) ([]domain.OutboxMessage, error) {
	rows, err := r.queries.ClaimOutboxBatch(ctx, ClaimOutboxBatchParams{
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
	// UPDATE ... RETURNING has no order; publish oldest first.
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
	if err := r.queries.MarkOutboxPublished(ctx, parsed); err != nil {
		return fmt.Errorf("mark outbox published: %w", err)
	}
	return nil
}
