// Package domain holds the Notification service's entities and errors. It
// imports the standard library only (AGENTS.md §4).
package domain

import (
	"encoding/json"
	"errors"
	"time"
)

// Events this service consumes (ARCHITECTURE.md §4.2). Booking publishes them;
// the names are the routing keys the queue is bound with.
const (
	EventBookingConfirmed   = "booking.confirmed"
	EventReservationExpired = "reservation.expired"
	EventBookingRefunded    = "booking.refunded"
)

// Envelope is the shape every event shares (ARCHITECTURE.md §4.2). The payload
// stays encoded until the event type says how to read it.
type Envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	OccurredAt    time.Time       `json:"occurred_at"`
	CorrelationID string          `json:"correlation_id"`
	Version       int             `json:"version"`
	Payload       json.RawMessage `json:"payload"`
}

// ChannelEmail is the only delivery channel so far.
const ChannelEmail = "email"

// Notification is one message to one user, produced from one event.
type Notification struct {
	// ID is the event_id the notification was produced from, which makes a
	// redelivered event find its notification instead of creating another.
	ID      string
	UserID  string
	Type    string
	Channel string
	Subject string
	Body    string
}

// ErrPermanent marks a message that no amount of retrying will deliver: it is
// malformed, of a type this service does not handle, or out of attempts. The
// consumer dead-letters it rather than requeueing it, so one poison message
// cannot block the queue. Anything else that goes wrong is transient.
var ErrPermanent = errors.New("permanent delivery failure")
