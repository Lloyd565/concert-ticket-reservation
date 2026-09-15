//go:build integration

package usecase_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/provider"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/usecase"
)

// Outbox tests (D9). In Payment the settle UPDATE and the outbox INSERT are one
// statement, so what these prove is that the event exists exactly when the
// settlement does: once per outcome, never for a duplicate that lost, and never
// for a charge that has no outcome yet.

type recordedEvent struct {
	Type    string
	Payload json.RawMessage
}

// outboxEvents reads an aggregate's events straight from the database.
func outboxEvents(t *testing.T, ctx context.Context, aggregateID string) []recordedEvent {
	t.Helper()
	rows, err := testPool.Query(ctx,
		`SELECT event_type, payload FROM outbox WHERE aggregate_id = $1 ORDER BY created_at`, aggregateID)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var out []recordedEvent
	for rows.Next() {
		var eventType string
		var body []byte
		if err := rows.Scan(&eventType, &body); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		var env struct {
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		out = append(out, recordedEvent{Type: eventType, Payload: env.Payload})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	return out
}

func TestOutboxRecordsPaymentSucceededOnceForAReplayedCharge(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeSucceed)
	in := newChargeInput()

	ch, err := f.charger.Charge(ctx, in)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if _, err := f.charger.Charge(ctx, in); err != nil {
		t.Fatalf("replayed charge: %v", err)
	}

	events := outboxEvents(t, ctx, ch.ID)
	if len(events) != 1 || events[0].Type != domain.EventPaymentSucceeded {
		t.Fatalf("want exactly one payment.succeeded, got %+v", events)
	}
	var p domain.ChargeSettledEvent
	if err := json.Unmarshal(events[0].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.ChargeID != ch.ID || p.ReservationID != in.ReservationID || p.AmountCents != in.AmountCents || p.Status != string(domain.ChargeSucceeded) {
		t.Fatalf("payment.succeeded does not describe the charge: %+v", p)
	}
}

// TestOutboxRecordsOneEventForConcurrentSettlements: several attempts under one
// key can each get an answer from the provider. One settles the charge; the rest
// lose the pending guard, and a loser must not announce an outcome either.
func TestOutboxRecordsOneEventForConcurrentSettlements(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeSucceed)
	in := newChargeInput()

	const callers = 8
	ids := make([]string, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if ch, err := f.charger.Charge(ctx, in); err == nil {
				ids[i] = ch.ID
			}
		}(i)
	}
	wg.Wait()

	if ids[0] == "" {
		t.Fatal("charge failed")
	}
	if events := outboxEvents(t, ctx, ids[0]); len(events) != 1 {
		t.Fatalf("want exactly one event for %d concurrent settlements of one charge, got %d", callers, len(events))
	}
}

func TestOutboxRecordsPaymentFailedForADecline(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeDecline)

	ch, err := f.charger.Charge(ctx, newChargeInput())
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	events := outboxEvents(t, ctx, ch.ID)
	if len(events) != 1 || events[0].Type != domain.EventPaymentFailed {
		t.Fatalf("want exactly one payment.failed, got %+v", events)
	}
	var p domain.ChargeSettledEvent
	if err := json.Unmarshal(events[0].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.DeclineReason == "" {
		t.Fatalf("payment.failed must say why: %+v", p)
	}
}

// TestOutboxRecordsNothingForAnUnsettledCharge is D8 on the event stream. A
// charge whose provider never answered has no outcome, and announcing one -
// failed or otherwise - would tell every consumer something nobody knows.
func TestOutboxRecordsNothingForAnUnsettledCharge(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeHang)
	in := newChargeInput()

	if _, err := f.charger.Charge(ctx, in); err == nil {
		t.Fatal("want an unknown outcome from a hanging provider")
	}
	var chargeID string
	if err := testPool.QueryRow(ctx, `SELECT id::text FROM charges WHERE idempotency_key = $1`, in.IdempotencyKey).Scan(&chargeID); err != nil {
		t.Fatalf("read charge: %v", err)
	}
	if events := outboxEvents(t, ctx, chargeID); len(events) != 0 {
		t.Fatalf("an unsettled charge announced an outcome: %+v", events)
	}
}

func TestOutboxRecordsRefundCompletedOnce(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeSucceed)
	in := newChargeInput()
	ch, err := f.charger.Charge(ctx, in)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}

	refundIn := usecase.RefundInput{ChargeID: ch.ID, AmountCents: in.AmountCents, IdempotencyKey: "refund-" + ch.ID}
	rf, err := f.refunder.Refund(ctx, refundIn)
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if _, err := f.refunder.Refund(ctx, refundIn); err != nil {
		t.Fatalf("replayed refund: %v", err)
	}

	events := outboxEvents(t, ctx, rf.ID)
	if len(events) != 1 || events[0].Type != domain.EventRefundCompleted {
		t.Fatalf("want exactly one refund.completed, got %+v", events)
	}
	var p domain.RefundCompletedEvent
	if err := json.Unmarshal(events[0].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.ChargeID != ch.ID || p.ReservationID != in.ReservationID || p.AmountCents != in.AmountCents {
		t.Fatalf("refund.completed does not describe the refund: %+v", p)
	}
}
