// Package events consumes the event bus (ARCHITECTURE.md §4.2).
//
// It owns this service's part of the broker topology - its queue, its bindings
// and its dead-letter queue - and turns the handler's verdict on each message
// into an ack, a requeue or a dead-letter. What a message means is decided in
// usecase, not here.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/lloyd565/concert-ticket-reservation/services/notification/internal/domain"
)

const (
	// Exchange is the durable topic exchange every service publishes to.
	Exchange = "concert.events"
	// Queue is this service's durable queue, and it outlives the service: while
	// Notification is down, events for it accumulate here and drain when it is
	// back. That is what lets Booking confirm with Notification dead (§3.3).
	Queue = "notification.events"
	// DeadLetterExchange and DeadLetterQueue take the messages this service will
	// never deliver, so one poison message cannot block everything behind it.
	DeadLetterExchange = "notification.dlx"
	DeadLetterQueue    = "notification.dlq"
)

const (
	dialTimeout    = 5 * time.Second
	reconnectDelay = 2 * time.Second
	// requeueDelay keeps a transient failure - the database is down - from
	// turning into a hot loop of immediate redeliveries.
	requeueDelay = 2 * time.Second
)

// routingKeys are the events Queue is bound to.
var routingKeys = []string{domain.EventBookingConfirmed, domain.EventReservationExpired, domain.EventBookingRefunded}

// Handler decides what a delivered event means. nil acknowledges it,
// domain.ErrPermanent dead-letters it, and any other error requeues it.
type Handler interface {
	Handle(ctx context.Context, env domain.Envelope) error
}

// Consumer feeds Queue to a Handler.
type Consumer struct {
	url     string
	handler Handler
	log     *slog.Logger
	ready   atomic.Bool
}

// NewConsumer wires a Consumer. It does not connect until Run.
func NewConsumer(url string, handler Handler, log *slog.Logger) *Consumer {
	return &Consumer{url: url, handler: handler, log: log}
}

// Ready reports whether the queue is declared and being consumed.
func (c *Consumer) Ready() bool { return c.ready.Load() }

// Run consumes until ctx is cancelled, reconnecting whenever the connection is
// lost. It returns once the message in hand, if any, has been settled.
func (c *Consumer) Run(ctx context.Context) {
	for {
		err := c.consume(ctx)
		if ctx.Err() != nil {
			return
		}
		c.log.WarnContext(ctx, "consumer disconnected; reconnecting", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}
}

func (c *Consumer) consume(ctx context.Context) error {
	conn, err := amqp.DialConfig(c.url, amqp.Config{
		Dial:      amqp.DefaultDial(dialTimeout),
		Heartbeat: 10 * time.Second,
		Locale:    "en_US",
	})
	if err != nil {
		return fmt.Errorf("dial broker: %w", err)
	}
	// Closing the connection hands every unacknowledged message back to the
	// queue, which is the graceful-shutdown behaviour §10 asks for.
	defer func() {
		if err := conn.Close(); err != nil && !conn.IsClosed() {
			c.log.Warn("closing broker connection", "error", err)
		}
	}()

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("open channel: %w", err)
	}
	if err := declare(ch); err != nil {
		return err
	}
	// One unacknowledged message at a time. It makes duplicates of one event
	// arrive one after another, which is what the processed_events check
	// relies on (see usecase.Notifier.Handle).
	if err := ch.Qos(1, 0, false); err != nil {
		return fmt.Errorf("set prefetch: %w", err)
	}
	deliveries, err := ch.ConsumeWithContext(ctx, Queue, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume %s: %w", Queue, err)
	}

	c.ready.Store(true)
	defer c.ready.Store(false)
	c.log.InfoContext(ctx, "consuming", "queue", Queue)

	for {
		select {
		case <-ctx.Done():
			return nil
		case d, ok := <-deliveries:
			if !ok {
				return errors.New("delivery channel closed")
			}
			c.settle(ctx, d)
		}
	}
}

// settle hands one delivery to the handler and acts on the verdict.
func (c *Consumer) settle(ctx context.Context, d amqp.Delivery) {
	var env domain.Envelope
	err := json.Unmarshal(d.Body, &env)
	if err != nil {
		err = fmt.Errorf("%w: undecodable message: %v", domain.ErrPermanent, err)
	} else {
		err = c.handler.Handle(ctx, env)
	}

	switch {
	case err == nil:
		// An ack that fails means the connection is gone. The broker redelivers
		// the message and the processed_events check makes that harmless.
		if ackErr := d.Ack(false); ackErr != nil {
			c.log.WarnContext(ctx, "ack failed; the message will be redelivered", "message_id", d.MessageId, "error", ackErr)
		}
	case errors.Is(err, domain.ErrPermanent):
		c.log.ErrorContext(ctx, "message dead-lettered", "message_id", d.MessageId, "routing_key", d.RoutingKey, "error", err)
		if nackErr := d.Nack(false, false); nackErr != nil {
			c.log.WarnContext(ctx, "dead-letter failed; the message will be redelivered", "message_id", d.MessageId, "error", nackErr)
		}
	default:
		c.log.WarnContext(ctx, "message requeued after a transient failure", "message_id", d.MessageId, "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(requeueDelay):
		}
		if nackErr := d.Nack(false, true); nackErr != nil {
			c.log.WarnContext(ctx, "requeue failed; the message will be redelivered", "message_id", d.MessageId, "error", nackErr)
		}
	}
}

// declare creates this service's topology. Every declaration is idempotent, and
// the exchange is declared by publishers too, so no start order matters.
//
// ponytail: a queue declared by its consumer does not exist until that consumer
// has started once, and events published before then are unroutable and
// dropped. Compose waits for this service to be ready, which covers every normal
// start; load the topology from a broker definitions file if Notification must
// be able to miss the very first boot.
func declare(ch *amqp.Channel) error {
	if err := ch.ExchangeDeclare(Exchange, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange %s: %w", Exchange, err)
	}
	if err := ch.ExchangeDeclare(DeadLetterExchange, amqp.ExchangeFanout, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange %s: %w", DeadLetterExchange, err)
	}
	if _, err := ch.QueueDeclare(DeadLetterQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare queue %s: %w", DeadLetterQueue, err)
	}
	if err := ch.QueueBind(DeadLetterQueue, "", DeadLetterExchange, false, nil); err != nil {
		return fmt.Errorf("bind %s: %w", DeadLetterQueue, err)
	}
	if _, err := ch.QueueDeclare(Queue, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange": DeadLetterExchange,
	}); err != nil {
		return fmt.Errorf("declare queue %s: %w", Queue, err)
	}
	for _, key := range routingKeys {
		if err := ch.QueueBind(Queue, key, Exchange, false, nil); err != nil {
			return fmt.Errorf("bind %s to %s: %w", Queue, key, err)
		}
	}
	return nil
}
