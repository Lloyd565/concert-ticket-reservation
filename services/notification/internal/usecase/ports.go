// Package usecase holds the Notification service's application logic. It
// depends on domain and on the interfaces declared below, never on their
// implementations, a database driver or the broker (AGENTS.md §4).
package usecase

import (
	"context"

	"github.com/lloyd565/concert-ticket-reservation/services/notification/internal/domain"
)

// NotificationRepository is the delivery-log port.
type NotificationRepository interface {
	// IsProcessed reports whether an event's notification has already been
	// sent.
	IsProcessed(ctx context.Context, eventID string) (bool, error)
	// StartNotification records a notification about to be attempted and
	// returns how many delivery attempts it has already had. A redelivered
	// event reuses its row.
	StartNotification(ctx context.Context, n domain.Notification) (int, error)
	// RecordFailedAttempt logs one attempt that did not deliver.
	RecordFailedAttempt(ctx context.Context, notificationID string, attemptNo int, sendErr error) error
	// MarkSent logs the delivering attempt, marks the notification sent and
	// records its event as processed, atomically.
	MarkSent(ctx context.Context, notificationID string, attemptNo int) error
	// MarkFailed records that a notification ran out of attempts.
	MarkFailed(ctx context.Context, notificationID string) error
}

// Mailer is the outbound email port.
type Mailer interface {
	// Send delivers one notification. Its ID is the idempotency key a real
	// provider would deduplicate a resend on.
	Send(ctx context.Context, n domain.Notification) error
}
