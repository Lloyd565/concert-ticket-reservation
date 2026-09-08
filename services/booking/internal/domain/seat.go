// Package domain holds entities, value objects, state machines and domain
// errors. It imports the standard library only (AGENTS.md §4): no drivers, no
// transport, no repository. IDs are plain strings here so that the layer stays
// free of any external type; parsing and formatting happen in repository.
package domain

import "time"

// SeatStatus is the lifecycle state of a single seat for a single event.
type SeatStatus string

const (
	SeatAvailable SeatStatus = "available"
	SeatHeld      SeatStatus = "held"
	SeatBooked    SeatStatus = "booked"
)

// seatTransitions is the seat state machine (ARCHITECTURE.md §5.3):
//
//	available ──hold──> held ──confirm──> booked ──refund──> available
//	    ▲                 │
//	    └──expire/release─┘
var seatTransitions = map[SeatStatus]map[SeatStatus]bool{
	SeatAvailable: {SeatHeld: true},
	SeatHeld:      {SeatBooked: true, SeatAvailable: true},
	SeatBooked:    {SeatAvailable: true},
}

// CanTransition reports whether the state machine connects from to to.
func (s SeatStatus) CanTransition(to SeatStatus) bool { return seatTransitions[s][to] }

// Seat is one physical seat for one event. It is the contended resource the
// whole system exists to protect.
type Seat struct {
	ID        string
	EventID   string
	Section   string
	Row       string
	Number    string
	Status    SeatStatus
	HeldBy    string     // reservation ID holding this seat; empty unless held
	HeldUntil *time.Time // expiry of the current hold; nil unless held
}

// Available reports whether the seat can currently be claimed.
func (s *Seat) Available() bool { return s.Status == SeatAvailable }

// Hold moves an available seat to held until the given deadline.
func (s *Seat) Hold(reservationID string, until time.Time) error {
	if err := s.transition(SeatHeld); err != nil {
		return err
	}
	s.HeldBy = reservationID
	u := until
	s.HeldUntil = &u
	return nil
}

// Confirm moves a held seat to booked. The hold metadata is cleared because a
// booked seat is claimed permanently, not until a deadline.
func (s *Seat) Confirm() error {
	if err := s.transition(SeatBooked); err != nil {
		return err
	}
	s.HeldUntil = nil
	return nil
}

// Release returns a held seat to available. It covers both explicit
// cancellation and hold expiry - the resulting state is identical.
func (s *Seat) Release() error { return s.toAvailable() }

// Refund returns a booked seat to available after a refund.
func (s *Seat) Refund() error { return s.toAvailable() }

func (s *Seat) toAvailable() error {
	if err := s.transition(SeatAvailable); err != nil {
		return err
	}
	s.HeldBy = ""
	s.HeldUntil = nil
	return nil
}

func (s *Seat) transition(to SeatStatus) error {
	if !s.Status.CanTransition(to) {
		return TransitionError{Entity: "seat", From: string(s.Status), To: string(to)}
	}
	s.Status = to
	return nil
}
