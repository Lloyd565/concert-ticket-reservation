package domain

import "time"

// Events Payment publishes (ARCHITECTURE.md §4.2). The type is also the
// RabbitMQ routing key.
const (
	EventPaymentSucceeded = "payment.succeeded"
	EventPaymentFailed    = "payment.failed"
	EventRefundCompleted  = "refund.completed"
)

// EventVersion is the envelope version every event is published with.
const EventVersion = 1

// Envelope is the shape every event shares on the wire (ARCHITECTURE.md §4.2).
// EventID is what consumers deduplicate on: delivery is at-least-once (D10).
type Envelope struct {
	EventID       string    `json:"event_id"`
	EventType     string    `json:"event_type"`
	OccurredAt    time.Time `json:"occurred_at"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	Version       int       `json:"version"`
	Payload       any       `json:"payload"`
}

// OutboxMessage is one committed, not-yet-confirmed event, as the relay sees it.
// Payload is the complete envelope, already encoded.
type OutboxMessage struct {
	ID        string
	EventType string
	Payload   []byte
	CreatedAt time.Time
}

// ChargeSettledEvent is the payload of payment.succeeded and payment.failed.
type ChargeSettledEvent struct {
	ChargeID      string `json:"charge_id"`
	ReservationID string `json:"reservation_id"`
	UserID        string `json:"user_id"`
	AmountCents   int64  `json:"amount_cents"`
	Status        string `json:"status"`
	DeclineReason string `json:"decline_reason,omitempty"`
}

// RefundCompletedEvent is the payload of refund.completed.
type RefundCompletedEvent struct {
	RefundID      string `json:"refund_id"`
	ChargeID      string `json:"charge_id"`
	ReservationID string `json:"reservation_id"`
	AmountCents   int64  `json:"amount_cents"`
}

// SettledEventType is the event a charge publishes when it settles. Only a
// settled charge has one: pending is not an outcome, and publishing it would be
// announcing a result nobody has.
func (c *Charge) SettledEventType() string {
	if c.Status == ChargeSucceeded {
		return EventPaymentSucceeded
	}
	return EventPaymentFailed
}
