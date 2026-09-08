package domain

import (
	"errors"
	"testing"
	"time"
)

func TestReservationLifecycle(t *testing.T) {
	r := &Reservation{Status: ReservationPending}
	if err := r.Confirm(); err != nil {
		t.Fatalf("confirm pending: %v", err)
	}
	if err := r.Refund(); err != nil {
		t.Fatalf("refund confirmed: %v", err)
	}
	if r.Status != ReservationRefunded {
		t.Fatalf("want refunded, got %s", r.Status)
	}
}

func TestReservationTerminalStatesAreOneWay(t *testing.T) {
	cases := []struct {
		name string
		from ReservationStatus
		act  func(*Reservation) error
	}{
		{"expired cannot confirm", ReservationExpired, func(r *Reservation) error { return r.Confirm() }},
		{"failed cannot confirm", ReservationFailed, func(r *Reservation) error { return r.Confirm() }},
		{"confirmed cannot expire", ReservationConfirmed, func(r *Reservation) error { return r.Expire() }},
		{"refunded cannot refund again", ReservationRefunded, func(r *Reservation) error { return r.Refund() }},
		{"pending cannot refund", ReservationPending, func(r *Reservation) error { return r.Refund() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Reservation{Status: tc.from}
			if err := tc.act(r); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("want ErrInvalidTransition, got %v", err)
			}
			if r.Status != tc.from {
				t.Fatalf("a rejected transition must not mutate state: %s -> %s", tc.from, r.Status)
			}
		})
	}
}

func TestReservationExpiredIsInclusiveOfTheDeadline(t *testing.T) {
	deadline := time.Now()
	r := &Reservation{ExpiresAt: deadline}
	if !r.Expired(deadline) {
		t.Fatal("a reservation is expired at its deadline, not one tick after")
	}
	if r.Expired(deadline.Add(-time.Nanosecond)) {
		t.Fatal("not expired before the deadline")
	}
}

func TestBookingRefund(t *testing.T) {
	b := &Booking{Status: BookingConfirmed}
	if err := b.Refund(); err != nil {
		t.Fatalf("refund confirmed booking: %v", err)
	}
	if err := b.Refund(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("double refund must be rejected, got %v", err)
	}
}
