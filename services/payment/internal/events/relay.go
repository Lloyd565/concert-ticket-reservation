// Package events publishes this service's outbox to RabbitMQ (D9,
// ARCHITECTURE.md §4.3). It is an outbound adapter: it imports domain and the
// broker driver, and reaches the database only through the Store port below.
package events

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/domain"
)

// Exchange is the durable topic exchange every service publishes to. The
// routing key is the event type.
const Exchange = "concert.events"

const (
	relayInterval = time.Second
	relayBatch    = 100
	// publishTimeout bounds one batch: every publish and every confirm in it.
	publishTimeout = 10 * time.Second
	// relayLease must outlast publishTimeout. A batch still being confirmed when
	// its lease lapses is claimable by another replica, which then publishes it
	// again - a duplicate rather than a loss, but a pointless one.
	relayLease  = 3 * publishTimeout
	dialTimeout = 5 * time.Second
)

// Store is the outbox as the relay needs it.
type Store interface {
	// ClaimOutbox leases up to limit unpublished events to the caller for
	// lease, oldest first, and commits the lease before returning.
	ClaimOutbox(ctx context.Context, limit int, lease time.Duration) ([]domain.OutboxMessage, error)
	// MarkOutboxPublished records that the broker confirmed these events.
	MarkOutboxPublished(ctx context.Context, ids []string) error
}

// Relay moves committed events from the outbox to the broker.
//
// It guarantees at-least-once publication: an event is marked published only
// after RabbitMQ has confirmed it, so every failure leaves the row to be
// published again, and a duplicate is the worst outcome (consumers are
// idempotent, D10). What it deliberately does not do is hold a transaction open
// while it talks to the broker (D4) - rows are leased by one short statement,
// published with nothing locked, and marked by another.
//
// Not safe for concurrent use: one Relay per process, driven by Run. Replicas
// each run their own and the leases keep them apart.
type Relay struct {
	url   string
	store Store
	log   *slog.Logger

	conn    *amqp.Connection
	ch      *amqp.Channel
	failing bool
}

// NewRelay wires a Relay. It does not connect: the broker is dialled on the
// first tick and after every failure, so Payment starts and serves with
// RabbitMQ down (ARCHITECTURE.md §3.3).
func NewRelay(url string, store Store, log *slog.Logger) *Relay {
	return &Relay{url: url, store: store, log: log}
}

// Run relays until ctx is cancelled.
//
// A broker outage never reaches a customer. Charges and refunds
// keep committing their events to the outbox, which is exactly where events
// should wait; this loop reconnects on the next tick and drains the backlog.
func (r *Relay) Run(ctx context.Context) {
	defer r.disconnect()
	ticker := time.NewTicker(relayInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// Keep going while batches come back full: there is more waiting.
		for {
			n, err := r.RelayOnce(ctx)
			if err != nil {
				// Logged on the transition, not on every tick of an outage.
				if !r.failing {
					r.log.WarnContext(ctx, "outbox relay cannot publish; events stay in the outbox until it can", "error", err)
				}
				r.failing = true
				break
			}
			if r.failing {
				r.log.InfoContext(ctx, "outbox relay publishing again")
				r.failing = false
			}
			if n < relayBatch {
				break
			}
		}
	}
}

// RelayOnce publishes one batch and returns how many events the broker
// confirmed. Exported so tests can drive it without the ticker.
func (r *Relay) RelayOnce(ctx context.Context) (int, error) {
	// Connect before claiming. A relay that cannot reach the broker must not
	// lease rows it could never send, or it holds them back from a replica that
	// can for the length of a lease.
	if err := r.connect(); err != nil {
		return 0, err
	}
	batch, err := r.store.ClaimOutbox(ctx, relayBatch, relayLease)
	if err != nil || len(batch) == 0 {
		return 0, err
	}

	pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()

	// Publish the whole batch, then collect the confirms. The broker acks
	// asynchronously; waiting on each before sending the next would make every
	// event pay a round trip for nothing.
	confirms := make([]*amqp.DeferredConfirmation, 0, len(batch))
	var failed error
	for _, m := range batch {
		dc, err := r.ch.PublishWithDeferredConfirmWithContext(pubCtx, Exchange, m.EventType, false, false, amqp.Publishing{
			ContentType: "application/json",
			// Persistent, so a broker restart does not drop what it confirmed.
			DeliveryMode: amqp.Persistent,
			MessageId:    m.ID,
			Type:         m.EventType,
			Timestamp:    m.CreatedAt,
			Body:         m.Payload,
		})
		if err != nil {
			failed = fmt.Errorf("publish %s: %w", m.ID, err)
			break
		}
		confirms = append(confirms, dc)
	}

	// Only an ack makes an event published. A nack, a timeout or a closed
	// channel leaves the row as it is, and its lease lapses into a retry.
	published := make([]string, 0, len(confirms))
	for i, dc := range confirms {
		acked, err := dc.WaitContext(pubCtx)
		if err != nil {
			failed = fmt.Errorf("await confirm for %s: %w", batch[i].ID, err)
			break
		}
		if !acked {
			// A nack, or the channel closed before the ack arrived.
			failed = fmt.Errorf("broker did not confirm %s", batch[i].ID)
			continue
		}
		published = append(published, batch[i].ID)
	}
	if failed != nil {
		// Start the next attempt from a fresh connection rather than reason
		// about what state this one is in.
		r.disconnect()
	}

	if len(published) > 0 {
		// The broker has these. Recorded on a detached context so a shutdown
		// arriving now does not turn confirmed events into duplicates.
		markCtx, cancelMark := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelMark()
		if err := r.store.MarkOutboxPublished(markCtx, published); err != nil {
			return 0, fmt.Errorf("mark %d confirmed events published: %w", len(published), err)
		}
	}
	return len(published), failed
}

func (r *Relay) connect() error {
	if r.ch != nil && !r.ch.IsClosed() {
		return nil
	}
	r.disconnect()

	conn, err := amqp.DialConfig(r.url, amqp.Config{
		Dial:      amqp.DefaultDial(dialTimeout),
		Heartbeat: 10 * time.Second,
		Locale:    "en_US",
	})
	if err != nil {
		return fmt.Errorf("dial broker: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		r.conn = conn
		r.disconnect()
		return fmt.Errorf("open channel: %w", err)
	}
	// Publisher confirms. Without them a publish reports success once its bytes
	// reach a socket buffer, and a broker that dies before persisting them loses
	// an event the outbox has already been told was sent.
	if err := ch.Confirm(false); err != nil {
		r.conn = conn
		r.disconnect()
		return fmt.Errorf("enable publisher confirms: %w", err)
	}
	// Idempotent, and declared by every service that touches the exchange, so
	// no start order between them matters.
	if err := ch.ExchangeDeclare(Exchange, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
		r.conn = conn
		r.disconnect()
		return fmt.Errorf("declare exchange %s: %w", Exchange, err)
	}
	r.conn, r.ch = conn, ch
	return nil
}

func (r *Relay) disconnect() {
	if r.conn != nil {
		if err := r.conn.Close(); err != nil && !r.conn.IsClosed() {
			r.log.Warn("closing broker connection", "error", err)
		}
	}
	r.conn, r.ch = nil, nil
}
