package domain

import (
	"errors"
	"testing"
	"time"
)

func TestSeatValidTransitions(t *testing.T) {
	until := time.Now().Add(10 * time.Minute)

	s := &Seat{Status: SeatAvailable}
	if err := s.Hold("res-1", until); err != nil {
		t.Fatalf("hold available seat: %v", err)
	}
	if s.Status != SeatHeld || s.HeldBy != "res-1" || s.HeldUntil == nil {
		t.Fatalf("hold did not record hold metadata: %+v", s)
	}
	if err := s.Confirm(); err != nil {
		t.Fatalf("confirm held seat: %v", err)
	}
	// Both pieces of hold metadata go: a booked seat is claimed permanently,
	// and the database CHECK requires a non-held seat to carry neither.
	if s.Status != SeatBooked || s.HeldUntil != nil || s.HeldBy != "" {
		t.Fatalf("confirm should clear the hold metadata: %+v", s)
	}
	if err := s.Refund(); err != nil {
		t.Fatalf("refund booked seat: %v", err)
	}
	if s.Status != SeatAvailable || s.HeldBy != "" {
		t.Fatalf("refund should clear hold metadata: %+v", s)
	}
}

func TestSeatReleaseReturnsHeldSeatToAvailable(t *testing.T) {
	s := &Seat{Status: SeatAvailable}
	if err := s.Hold("res-1", time.Now()); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := s.Release(); err != nil {
		t.Fatalf("release held seat: %v", err)
	}
	if s.Status != SeatAvailable || s.HeldBy != "" || s.HeldUntil != nil {
		t.Fatalf("release should clear hold metadata: %+v", s)
	}
}

func TestSeatIllegalTransitionsAreTypedErrorsNotNoOps(t *testing.T) {
	cases := []struct {
		name string
		seat Seat
		act  func(*Seat) error
	}{
		{"double hold", Seat{Status: SeatHeld}, func(s *Seat) error { return s.Hold("res-2", time.Now()) }},
		{"hold a booked seat", Seat{Status: SeatBooked}, func(s *Seat) error { return s.Hold("res-2", time.Now()) }},
		{"confirm an available seat", Seat{Status: SeatAvailable}, func(s *Seat) error { return s.Confirm() }},
		{"confirm a booked seat", Seat{Status: SeatBooked}, func(s *Seat) error { return s.Confirm() }},
		{"release an available seat", Seat{Status: SeatAvailable}, func(s *Seat) error { return s.Release() }},
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
