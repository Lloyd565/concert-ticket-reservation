package domain

import (
	"errors"
	"testing"
)

func TestChargeValidTransitions(t *testing.T) {
	c := &Charge{Status: ChargePending}
	if err := c.Succeed("ch_123"); err != nil {
		t.Fatalf("succeed a pending charge: %v", err)
	}
	if c.Status != ChargeSucceeded || c.ProviderRef != "ch_123" {
		t.Fatalf("succeed must record the provider reference: %+v", c)
	}
	if !c.Refundable() {
		t.Fatal("a succeeded charge must be refundable")
	}

	d := &Charge{Status: ChargePending}
	if err := d.Decline("insufficient funds"); err != nil {
		t.Fatalf("decline a pending charge: %v", err)
	}
	if d.Refundable() {
		t.Fatal("a declined charge took no money and must not be refundable")
	}
}

// TestChargeCannotSettleWithoutProviderReference: a succeeded charge with
// nothing to refund against is unrefundable, and the database rejects it too.
func TestChargeCannotSettleWithoutProviderReference(t *testing.T) {
	c := &Charge{Status: ChargePending}
	if err := c.Succeed(""); !errors.Is(err, ErrMissingProviderRef) {
		t.Fatalf("want ErrMissingProviderRef, got %v", err)
	}
	if c.Status != ChargePending {
		t.Fatalf("a rejected settlement must leave the charge pending, got %s", c.Status)
	}
}

// TestSettledChargesAreTerminal is the property the whole idempotency story
// rests on: once a charge has an answer, no later call may change it.
func TestSettledChargesAreTerminal(t *testing.T) {
	cases := []struct {
		name string
		from ChargeStatus
		act  func(*Charge) error
	}{
		{"succeed a succeeded charge", ChargeSucceeded, func(c *Charge) error { return c.Succeed("ch_2") }},
		{"decline a succeeded charge", ChargeSucceeded, func(c *Charge) error { return c.Decline("too late") }},
		{"succeed a declined charge", ChargeDeclined, func(c *Charge) error { return c.Succeed("ch_2") }},
		{"fail a declined charge", ChargeDeclined, func(c *Charge) error { return c.Fail("too late") }},
		{"succeed a failed charge", ChargeFailed, func(c *Charge) error { return c.Succeed("ch_2") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Charge{Status: tc.from, ProviderRef: "ch_1"}
			err := tc.act(c)
			if !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("want ErrInvalidTransition, got %v", err)
			}
			// Not a silent no-op: the state and the reference are untouched, so
			// a caller cannot be left believing money moved when it did not.
			if c.Status != tc.from || c.ProviderRef != "ch_1" {
				t.Fatalf("illegal transition mutated the charge: %+v", c)
			}
			if !tc.from.Terminal() {
				t.Fatalf("%s should be terminal", tc.from)
			}
		})
	}
}

// TestPendingIsNotTerminal guards the distinction the whole saga depends on:
// pending is an unknown outcome, not a settled one.
func TestPendingIsNotTerminal(t *testing.T) {
	if ChargePending.Terminal() {
		t.Fatal("pending must not be terminal: it means the outcome is unknown, not that it failed")
	}
	if !ChargeFailed.Terminal() {
		t.Fatal("failed must be terminal")
	}
}

func TestRefundTransitions(t *testing.T) {
	r := &Refund{Status: RefundPending}
	if err := r.Succeed("re_1"); err != nil {
		t.Fatalf("succeed a pending refund: %v", err)
	}
	if err := r.Fail(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a settled refund must not be re-settled, got %v", err)
	}
	if r.Status != RefundSucceeded || r.ProviderRef != "re_1" {
		t.Fatalf("illegal transition mutated the refund: %+v", r)
	}
	if err := (&Refund{Status: RefundPending}).Succeed(""); !errors.Is(err, ErrMissingProviderRef) {
		t.Fatalf("want ErrMissingProviderRef, got %v", err)
	}
}
