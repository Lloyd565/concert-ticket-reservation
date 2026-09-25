package domain

import "time"

// BookingStatus is the lifecycle of a confirmed purchase. A booking only comes
// into existence once payment has succeeded, so there is no pending state here
// - pending lives on Reservation.
type BookingStatus string

const (
	BookingConfirmed BookingStatus = "confirmed"
	BookingRefunded  BookingStatus = "refunded"
)

var bookingTransitions = map[BookingStatus]map[BookingStatus]bool{
	BookingConfirmed: {BookingRefunded: true},
}

// CanTransition reports whether the state machine connects from to to.
func (s BookingStatus) CanTransition(to BookingStatus) bool { return bookingTransitions[s][to] }

// Booking is the durable record of a completed purchase.
//
// P0 note: nothing constructs a Booking yet - confirmation arrives with the
// Payment service in P2. The entity and its transition live here now so the
// seat state machine has a counterpart to move against, and so the rules stay
// in domain rather than being invented in a handler later (AGENTS.md §4).
type Booking struct {
	ID            string
	ReservationID string
	UserID        string
	PaymentID     string
	Status        BookingStatus
	ConfirmedAt   time.Time
}

// Refund moves a confirmed booking to refunded.
func (b *Booking) Refund() error {
	if !b.Status.CanTransition(BookingRefunded) {
		return TransitionError{Entity: "booking", From: string(b.Status), To: string(BookingRefunded)}
	}
	b.Status = BookingRefunded
	return nil
}
