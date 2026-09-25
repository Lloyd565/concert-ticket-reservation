package domain

import (
	"errors"
	"testing"
)

func TestSeatValidTransitions(t *testing.T) {
	// The catalog row has two states in P3: the held state between them lives
	// in Redis, so redeeming a hold is available -> booked here.
	s := &Seat{Status: SeatAvailable}
	if err := s.Confirm(); err != nil {
		t.Fatalf("confirm available seat: %v", err)
	}
	if s.Status != SeatBooked {
		t.Fatalf("confirm should book the seat, got %s", s.Status)
	}
	if err := s.Refund(); err != nil {
		t.Fatalf("refund booked seat: %v", err)
	}
	if s.Status != SeatAvailable {
		t.Fatalf("refund should free the seat, got %s", s.Status)
	}
}

func TestSeatAvailableOnlyMeansUnsold(t *testing.T) {
	// A seat somebody is holding right now still reads available here. That is
	// not a bug to fix by adding a third state: a hold that Postgres records is
	// a hold nothing expires without a sweeper, which is what P3 retires.
	s := &Seat{Status: SeatAvailable}
	if !s.Available() {
		t.Fatal("an unsold seat must be available")
	}
	if err := s.Confirm(); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if s.Available() {
		t.Fatal("a booked seat must not be available")
	}
}

func TestSeatIllegalTransitionsAreTypedErrorsNotNoOps(t *testing.T) {
	cases := []struct {
		name string
		seat Seat
		act  func(*Seat) error
	}{
		{"confirm a booked seat", Seat{Status: SeatBooked}, func(s *Seat) error { return s.Confirm() }},
		{"refund an available seat", Seat{Status: SeatAvailable}, func(s *Seat) error { return s.Refund() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seat := tc.seat
			before := seat.Status
			err := tc.act(&seat)
			if err == nil {
				t.Fatal("expected an error, got nil (a silent no-op is a defect)")
			}
			if !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("want ErrInvalidTransition, got %v", err)
			}
			var te TransitionError
			if !errors.As(err, &te) || te.Entity != "seat" {
				t.Fatalf("want a seat TransitionError, got %#v", err)
			}
			if seat.Status != before {
				t.Fatalf("a rejected transition must not mutate state: %s -> %s", before, seat.Status)
			}
		})
	}
}
