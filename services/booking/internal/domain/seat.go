// Package domain holds entities, value objects, state machines and domain
// errors. It imports the standard library only (AGENTS.md §4): no drivers, no
// transport, no repository. IDs are plain strings here so that the layer stays
// free of any external type; parsing and formatting happen in repository.
package domain

// SeatStatus is the lifecycle state of a single seat for a single event, as the
// seat catalog records it.
//
// There are two values, not three, and the missing one is the interesting one.
// The full state machine (ARCHITECTURE.md §5.3) is:
//
//	available ──hold──> held ──confirm──> booked ──refund──> available
//	    ▲                 │
//	    └──expire/release─┘
//
// In P3 the held state moved to Redis, where a key with a TTL expires itself
// (D3, ARCHITECTURE.md §5.2). It is deliberately not representable here: a hold
// recorded in Postgres is a hold nothing can expire without a sweeper, and the
// sweeper is what P3 retires. So the catalog row goes straight from available to
// booked when a hold is redeemed, and the intermediate state is a Redis key that
// this layer never sees.
type SeatStatus string

const (
	SeatAvailable SeatStatus = "available"
	SeatBooked    SeatStatus = "booked"
)

// seatTransitions is what a seat catalog row may do. Holding is absent for the
// reason above; confirming a hold is available -> booked.
var seatTransitions = map[SeatStatus]map[SeatStatus]bool{
	SeatAvailable: {SeatBooked: true},
	SeatBooked:    {SeatAvailable: true},
}

// CanTransition reports whether the state machine connects from to to.
func (s SeatStatus) CanTransition(to SeatStatus) bool { return seatTransitions[s][to] }

// Seat is one physical seat for one event. It is the contended resource the
// whole system exists to protect.
type Seat struct {
	ID      string
	EventID string
	Section string
	Row     string
	Number  string
	Status  SeatStatus
	// PriceCents is what this seat costs. Integer cents; money is never a
	// float (AGENTS.md §5).
	PriceCents int64
}

// Available reports whether the seat can still be sold. A seat somebody is
// holding in Redis right now is available by this answer - the hold is checked
// where holds live, not here.
func (s *Seat) Available() bool { return s.Status == SeatAvailable }

// Confirm moves an available seat to booked: the moment a transient hold
// becomes a permanent sale.
func (s *Seat) Confirm() error { return s.transition(SeatBooked) }

// Refund returns a booked seat to available after a refund.
func (s *Seat) Refund() error { return s.transition(SeatAvailable) }

func (s *Seat) transition(to SeatStatus) error {
	if !s.Status.CanTransition(to) {
		return TransitionError{Entity: "seat", From: string(s.Status), To: string(to)}
	}
	s.Status = to
	return nil
}
