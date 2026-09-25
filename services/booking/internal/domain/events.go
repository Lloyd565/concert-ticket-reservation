package domain

import "time"

// Events Booking publishes (ARCHITECTURE.md §4.2). The type is also the
// RabbitMQ routing key.
const (
	EventReservationHeld    = "reservation.held"
	EventReservationExpired = "reservation.expired"
	EventBookingConfirmed   = "booking.confirmed"
	EventBookingRefunded    = "booking.refunded"
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

// ReservationHeldEvent is the payload of reservation.held.
type ReservationHeldEvent struct {
	ReservationID string    `json:"reservation_id"`
	UserID        string    `json:"user_id"`
	EventID       string    `json:"concert_event_id"`
	SeatIDs       []string  `json:"seat_ids"`
	TotalCents    int64     `json:"total_cents"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// ReservationExpiredEvent is the payload of reservation.expired.
type ReservationExpiredEvent struct {
	ReservationID string    `json:"reservation_id"`
	UserID        string    `json:"user_id"`
	EventID       string    `json:"concert_event_id"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// BookingConfirmedEvent is the payload of booking.confirmed: everything Notification
// needs to send the tickets without calling back into Booking.
type BookingConfirmedEvent struct {
	BookingID     string         `json:"booking_id"`
	ReservationID string         `json:"reservation_id"`
	UserID        string         `json:"user_id"`
	EventID       string         `json:"concert_event_id"`
	PaymentID     string         `json:"payment_id"`
	TotalCents    int64          `json:"total_cents"`
	Tickets       []TicketIssued `json:"tickets"`
}

// TicketIssued is one ticket inside booking.confirmed.
type TicketIssued struct {
	TicketID string `json:"ticket_id"`
	SeatID   string `json:"seat_id"`
	QRCode   string `json:"qr_code"`
}

// BookingRefundedEvent is the payload of booking.refunded.
type BookingRefundedEvent struct {
	BookingID     string `json:"booking_id"`
	ReservationID string `json:"reservation_id"`
	UserID        string `json:"user_id"`
}

// LapsedReservation is a reservation the expirer has just moved to expired.
type LapsedReservation struct {
	ID        string
	UserID    string
	EventID   string
	ExpiresAt time.Time
}
