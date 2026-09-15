// Package postgres implements the repository interface declared in usecase. It
// is the only package in the service that imports a database driver
// (AGENTS.md §4). Domain entities carry string IDs; parsing happens here.
package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lloyd565/concert-ticket-reservation/services/notification/internal/domain"
)

// Repo implements usecase.NotificationRepository.
//
// There is no TxManager. The one write that must be atomic across tables -
// sent, attempt and processed_events together - is a single statement
// (MarkNotificationSent), and a single statement already is.
type Repo struct{ queries *Queries }

// NewRepo returns a Repo backed by pool.
func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{queries: New(pool)} }

// IsProcessed reports whether an event's notification has already been sent.
func (r *Repo) IsProcessed(ctx context.Context, eventID string) (bool, error) {
	id, err := uuid.Parse(eventID)
	if err != nil {
		return false, fmt.Errorf("parse event id %q: %w", eventID, err)
	}
	done, err := r.queries.IsEventProcessed(ctx, id)
	if err != nil {
		return false, fmt.Errorf("check processed event: %w", err)
	}
	return done, nil
}

// StartNotification records a notification about to be attempted and returns
// how many attempts it has already had.
func (r *Repo) StartNotification(ctx context.Context, n domain.Notification) (int, error) {
	id, err := uuid.Parse(n.ID)
	if err != nil {
		return 0, fmt.Errorf("parse notification id %q: %w", n.ID, err)
	}
	userID, err := uuid.Parse(n.UserID)
	if err != nil {
		return 0, fmt.Errorf("parse user id %q: %w", n.UserID, err)
	}
	payload, err := json.Marshal(map[string]string{"subject": n.Subject, "body": n.Body})
	if err != nil {
		return 0, fmt.Errorf("encode notification: %w", err)
	}
	prior, err := r.queries.StartNotification(ctx, StartNotificationParams{
		ID:      id,
		UserID:  userID,
		Type:    n.Type,
		Channel: n.Channel,
		Payload: payload,
	})
	if err != nil {
		return 0, fmt.Errorf("start notification: %w", err)
	}
	return int(prior), nil
}

// RecordFailedAttempt logs one attempt that did not deliver.
func (r *Repo) RecordFailedAttempt(ctx context.Context, notificationID string, attemptNo int, sendErr error) error {
	id, err := uuid.Parse(notificationID)
	if err != nil {
		return fmt.Errorf("parse notification id %q: %w", notificationID, err)
	}
	reason := sendErr.Error()
	if err := r.queries.RecordFailedAttempt(ctx, RecordFailedAttemptParams{
		ID:             uuid.Must(uuid.NewV7()),
		NotificationID: id,
		AttemptNo:      int32(attemptNo), //nolint:gosec // bounded by the retry limit
		Error:          &reason,
	}); err != nil {
		return fmt.Errorf("record failed attempt: %w", err)
	}
	return nil
}

// MarkSent logs the delivering attempt, marks the notification sent and records
// its event as processed, in one statement.
func (r *Repo) MarkSent(ctx context.Context, notificationID string, attemptNo int) error {
	id, err := uuid.Parse(notificationID)
	if err != nil {
		return fmt.Errorf("parse notification id %q: %w", notificationID, err)
	}
	if err := r.queries.MarkNotificationSent(ctx, MarkNotificationSentParams{
		AttemptID:      uuid.Must(uuid.NewV7()),
		NotificationID: id,
		AttemptNo:      int32(attemptNo), //nolint:gosec // bounded by the retry limit
	}); err != nil {
		return fmt.Errorf("mark notification sent: %w", err)
	}
	return nil
}

// MarkFailed records that a notification ran out of attempts.
func (r *Repo) MarkFailed(ctx context.Context, notificationID string) error {
	id, err := uuid.Parse(notificationID)
	if err != nil {
		return fmt.Errorf("parse notification id %q: %w", notificationID, err)
	}
	if err := r.queries.MarkNotificationFailed(ctx, id); err != nil {
		return fmt.Errorf("mark notification failed: %w", err)
	}
	return nil
}
