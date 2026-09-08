package domain

import "time"

// Ticket is the attendee's proof of a seat: one ticket per seat per booking
// (PRD §4.1 step 6).
type Ticket struct {
	ID        string
	BookingID string
	SeatID    string
	// QRCode is what gets scanned at the door. It is unique across the system,
	// because two valid tickets carrying the same code is the physical-world
	// version of the double-booking this project exists to prevent.
	QRCode   string
	IssuedAt time.Time
}
