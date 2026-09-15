package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/lloyd565/concert-ticket-reservation/services/notification/internal/domain"
)

// Retry defaults, used when the caller passes zero.
const (
	DefaultMaxAttempts = 5
	// DefaultBackoff doubles per attempt: 0.5s, 1s, 2s, 4s between five
	// attempts, all while the message stays unacknowledged.
	DefaultBackoff = 500 * time.Millisecond
)

// Notifier turns events into emails.
type Notifier struct {
	repo        NotificationRepository
	mailer      Mailer
	maxAttempts int
	backoff     time.Duration
	log         *slog.Logger
}

// NewNotifier wires a Notifier. Zero values fall back to the Default constants.
func NewNotifier(repo NotificationRepository, mailer Mailer, maxAttempts int, backoff time.Duration, log *slog.Logger) *Notifier {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	if backoff <= 0 {
		backoff = DefaultBackoff
	}
	return &Notifier{repo: repo, mailer: mailer, maxAttempts: maxAttempts, backoff: backoff, log: log}
}

// Handle turns one delivered event into at most one sent email.
//
// Delivery is at-least-once, twice over: the relay republishes an event the
// broker confirmed but Booking died before recording, and the broker redelivers
// anything this service did not acknowledge. Duplicates are the normal case, not
// an edge case (D10). So the first thing asked of every message is whether its
// event has been processed - and "processed" is written by the same statement
// that records the email as sent, so the two cannot disagree.
//
// The error decides what happens to the message: nil acknowledges it,
// domain.ErrPermanent dead-letters it, and anything else - the database is
// down, the service is shutting down - hands it back to the queue.
//
// ponytail: duplicates are caught when they arrive one after another, which is
// what one consumer with prefetch 1 guarantees. Two replicas could each take a
// copy of the same event at the same instant and both send; scale out with a
// lease on the notification row first (the outbox relay's pattern).
func (n *Notifier) Handle(ctx context.Context, env domain.Envelope) error {
	if _, err := uuid.Parse(env.EventID); err != nil {
		// Checked before the database sees it: an event_id that is not a UUID
		// would fail every query as a "database error" and be requeued forever.
		return fmt.Errorf("%w: event_id %q is not a uuid", domain.ErrPermanent, env.EventID)
	}
	log := n.log.With("event_id", env.EventID, "event_type", env.EventType, "correlation_id", env.CorrelationID)

	processed, err := n.repo.IsProcessed(ctx, env.EventID)
	if err != nil {
		return err
	}
	if processed {
		log.InfoContext(ctx, "duplicate event; its notification was already sent")
		return nil
	}

	msg, err := compose(env)
	if err != nil {
		return fmt.Errorf("%w: event %s: %v", domain.ErrPermanent, env.EventID, err)
	}
	prior, err := n.repo.StartNotification(ctx, msg)
	if err != nil {
		return err
	}

	for attempt := prior + 1; ; attempt++ {
		sendErr := n.mailer.Send(ctx, msg)
		if sendErr == nil {
			// Recorded even if the service is shutting down: the email has gone,
			// and forgetting that is how a redelivery sends it twice.
			// ponytail: a crash between Send and this write still resends once on
			// redelivery. Closing that needs the provider to honour msg.ID as an
			// idempotency key, as Payment's provider does.
			markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := n.repo.MarkSent(markCtx, msg.ID, attempt); err != nil {
				return err
			}
			log.InfoContext(ctx, "notification sent", "notification_id", msg.ID, "attempt", attempt)
			return nil
		}

		if err := n.repo.RecordFailedAttempt(ctx, msg.ID, attempt, sendErr); err != nil {
			return err
		}
		if attempt >= n.maxAttempts {
			if err := n.repo.MarkFailed(ctx, msg.ID); err != nil {
				return err
			}
			return fmt.Errorf("%w: notification %s failed %d attempts: %v", domain.ErrPermanent, msg.ID, attempt, sendErr)
		}
		log.WarnContext(ctx, "notification delivery failed; retrying", "attempt", attempt, "error", sendErr)

		// Exponential backoff with jitter. The shift is capped so a notification
		// replayed many times cannot overflow its way into a zero delay.
		delay := n.backoff << min(attempt-1, 10)
		jitter := time.Duration(rand.Int63n(int64(delay)/2 + 1)) //nolint:gosec // spreading retries, not generating secrets
		select {
		case <-ctx.Done():
			return fmt.Errorf("retry of notification %s interrupted: %w", msg.ID, ctx.Err())
		case <-time.After(delay + jitter):
		}
	}
}

// eventPayload is the union of the fields the three consumed events carry. Their
// field names do not collide, so one struct reads all of them.
type eventPayload struct {
	UserID        string    `json:"user_id"`
	ReservationID string    `json:"reservation_id"`
	BookingID     string    `json:"booking_id"`
	TotalCents    int64     `json:"total_cents"`
	ExpiresAt     time.Time `json:"expires_at"`
	Tickets       []struct {
		SeatID string `json:"seat_id"`
		QRCode string `json:"qr_code"`
	} `json:"tickets"`
}

// compose renders the email an event calls for. An error is permanent: the same
// bytes will fail the same way on every redelivery.
func compose(env domain.Envelope) (domain.Notification, error) {
	var p eventPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return domain.Notification{}, fmt.Errorf("decode %s payload: %w", env.EventType, err)
	}
	if _, err := uuid.Parse(p.UserID); err != nil {
		return domain.Notification{}, fmt.Errorf("%s payload has no valid user_id %q", env.EventType, p.UserID)
	}
	n := domain.Notification{ID: env.EventID, UserID: p.UserID, Type: env.EventType, Channel: domain.ChannelEmail}

	switch env.EventType {
	case domain.EventBookingConfirmed:
		if len(p.Tickets) == 0 {
			return domain.Notification{}, errors.New("booking.confirmed carries no tickets")
		}
		codes := make([]string, 0, len(p.Tickets))
		for _, t := range p.Tickets {
			codes = append(codes, t.QRCode)
		}
		n.Subject = "Your tickets are confirmed"
		n.Body = fmt.Sprintf("Booking %s is confirmed: %d ticket(s), %s paid. Ticket codes: %s",
			p.BookingID, len(p.Tickets), money(p.TotalCents), strings.Join(codes, ", "))
	case domain.EventReservationExpired:
		n.Subject = "Your seat hold has expired"
		n.Body = fmt.Sprintf("Reservation %s expired at %s without payment, and its seats have been released.",
			p.ReservationID, p.ExpiresAt.UTC().Format(time.RFC3339))
	case domain.EventBookingRefunded:
		n.Subject = "Your booking has been refunded"
		n.Body = fmt.Sprintf("Booking %s has been refunded and its tickets are no longer valid.", p.BookingID)
	default:
		return domain.Notification{}, fmt.Errorf("unhandled event type %q", env.EventType)
	}
	return n, nil
}

// money formats integer cents. Never a float (AGENTS.md §5).
func money(cents int64) string { return fmt.Sprintf("$%d.%02d", cents/100, cents%100) }
