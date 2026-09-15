//go:build integration

package usecase_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
)

// Outbox and expiry tests (D9, ARCHITECTURE.md §4.3).
//
// The property under test is not that an event row appears. It is that the row
// commits with the state change it describes and under no other circumstance:
// written in the same transaction, rolled back with it, and never twice for one
// change. Real Postgres, for the reason every test here uses it - a mocked
// repository would agree with whatever the code believed about transactions.

type recordedEvent struct {
	EventID   string          `json:"event_id"`
	EventType string          `json:"event_type"`
	Version   int             `json:"version"`
	Payload   json.RawMessage `json:"payload"`
}

// outboxEvents reads an aggregate's events of one type straight from the
// database, oldest first.
func outboxEvents(t *testing.T, ctx context.Context, aggregateID, eventType string) []recordedEvent {
	t.Helper()
	rows, err := testPool.Query(ctx,
		`SELECT payload FROM outbox WHERE aggregate_id = $1 AND event_type = $2 ORDER BY created_at`, aggregateID, eventType)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var out []recordedEvent
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			t.Fatalf("scan outbox row: %v", err)
		}
		var ev recordedEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	return out
}

// onlyEvent asserts an aggregate recorded exactly one event of a type, checks its
// envelope, and decodes its payload into v.
func onlyEvent(t *testing.T, ctx context.Context, aggregateID, eventType string, v any) {
	t.Helper()
	events := outboxEvents(t, ctx, aggregateID, eventType)
	if len(events) != 1 {
		t.Fatalf("want exactly 1 %s for %s, got %d", eventType, aggregateID, len(events))
	}
	if events[0].EventType != eventType || events[0].Version != domain.EventVersion {
		t.Fatalf("envelope does not match its row: %+v", events[0])
	}
	if _, err := uuid.Parse(events[0].EventID); err != nil {
		t.Fatalf("an event_id consumers can deduplicate on is required, got %q", events[0].EventID)
	}
	if err := json.Unmarshal(events[0].Payload, v); err != nil {
		t.Fatalf("decode %s payload: %v", eventType, err)
	}
}

// lapse moves a reservation's checkout deadline into the past.
func lapse(t *testing.T, ctx context.Context, reservationID string) {
	t.Helper()
	tag, err := testPool.Exec(ctx, `UPDATE reservations SET expires_at = now() - interval '1 minute' WHERE id = $1`, reservationID)
	if err != nil {
		t.Fatalf("lapse reservation: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("reservation %s not found", reservationID)
	}
}

// expireAll runs the expirer until it finds nothing. The scan is global, so a
// single pass may be spent on other tests' lapsed reservations.
func expireAll(t *testing.T, ctx context.Context, f *fixture) {
	t.Helper()
	for range 100 {
		n, err := f.expirer.ExpireOnce(ctx)
		if err != nil {
			t.Fatalf("expire: %v", err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("the expirer never ran out of reservations to expire")
}

func TestOutboxRecordsReservationHeldWithTheHold(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	eventID, seatID := seedOneSeat(t, f)
	userID := uuid.Must(uuid.NewV7()).String()
	key := uuid.Must(uuid.NewV7()).String()

	held, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, userID, key)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	// A replayed hold returns the first reservation. It changed nothing, so it
	// must announce nothing.
	if _, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, userID, key); err != nil {
		t.Fatalf("replayed hold: %v", err)
	}

	var p domain.ReservationHeldEvent
	onlyEvent(t, ctx, held.ReservationID, domain.EventReservationHeld, &p)
	if p.ReservationID != held.ReservationID || p.UserID != userID || p.EventID != eventID ||
		len(p.SeatIDs) != 1 || p.SeatIDs[0] != seatID || p.TotalCents != held.TotalCents {
		t.Fatalf("reservation.held does not describe the hold: %+v", p)
	}
}

// TestOutboxRecordsBookingConfirmedWithTheBooking is the event Notification
// sends tickets from, so it has to carry them.
func TestOutboxRecordsBookingConfirmedWithTheBooking(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	_, _, userID, held := hold(t, f)

	conf, err := f.saga.Pay(ctx, held.ReservationID, userID, "pay-once")
	if err != nil {
		t.Fatalf("pay: %v", err)
	}
	// The replay answers from the existing booking; a second event here would be
	// a second ticket email.
	if _, err := f.saga.Pay(ctx, held.ReservationID, userID, "pay-again"); err != nil {
		t.Fatalf("replayed pay: %v", err)
	}

	var p domain.BookingConfirmedEvent
	onlyEvent(t, ctx, held.ReservationID, domain.EventBookingConfirmed, &p)
	if p.BookingID != conf.Booking.ID || p.PaymentID != conf.Booking.PaymentID || p.UserID != userID || p.TotalCents != held.TotalCents {
		t.Fatalf("booking.confirmed does not describe the booking: %+v", p)
	}
	if len(p.Tickets) != 1 || p.Tickets[0].QRCode != conf.Tickets[0].QRCode {
		t.Fatalf("booking.confirmed must carry the tickets it announces: %+v", p.Tickets)
	}
}

func TestOutboxRecordsBookingRefundedWithTheRefund(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	_, _, userID, held := hold(t, f)
	conf, err := f.saga.Pay(ctx, held.ReservationID, userID, "pay")
	if err != nil {
		t.Fatalf("pay: %v", err)
	}

	for range 2 {
		if err := f.saga.ReleaseRefunded(ctx, held.ReservationID); err != nil {
			t.Fatalf("release refunded: %v", err)
		}
	}

	var p domain.BookingRefundedEvent
	onlyEvent(t, ctx, held.ReservationID, domain.EventBookingRefunded, &p)
	if p.BookingID != conf.Booking.ID || p.UserID != userID {
		t.Fatalf("booking.refunded does not describe the refund: %+v", p)
	}
}

// TestOutboxWriteRollsBackWithItsTransaction is D9 itself.
//
// If Enqueue wrote through the pool instead of the caller's transaction, the
// event would survive the rollback below - an announcement of a state change
// that never happened - and the same mistake, the other way round, is a booking
// whose event a crash can lose. And with no transaction to join at all, the write
// must be refused rather than committed on its own.
func TestOutboxWriteRollsBackWithItsTransaction(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	aggregateID := uuid.Must(uuid.NewV7()).String()
	stateChangeFailed := errors.New("the state change failed")

	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := f.repo.Enqueue(ctx, domain.EventReservationHeld, aggregateID, map[string]string{}); err != nil {
			return err
		}
		return stateChangeFailed
	})
	if !errors.Is(err, stateChangeFailed) {
		t.Fatalf("want the transaction's own error, got %v", err)
	}
	if n := len(outboxEvents(t, ctx, aggregateID, domain.EventReservationHeld)); n != 0 {
		t.Fatalf("an event outlived the transaction it was written in: %d rows", n)
	}

	if err := f.repo.Enqueue(ctx, domain.EventReservationHeld, aggregateID, map[string]string{}); err == nil {
		t.Fatal("an outbox write outside a transaction must be refused")
	}
	if n := len(outboxEvents(t, ctx, aggregateID, domain.EventReservationHeld)); n != 0 {
		t.Fatalf("an outbox write outside a transaction was committed: %d rows", n)
	}
}

func TestExpirerExpiresALapsedReservationWithItsEvent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	_, _, userID, held := hold(t, f)

	lapse(t, ctx, held.ReservationID)
	expireAll(t, ctx, f)

	if status, _ := reservationState(t, ctx, held.ReservationID); status != string(domain.ReservationExpired) {
		t.Fatalf("want the lapsed reservation expired, got %s", status)
	}
	var p domain.ReservationExpiredEvent
	onlyEvent(t, ctx, held.ReservationID, domain.EventReservationExpired, &p)
	if p.ReservationID != held.ReservationID || p.UserID != userID {
		t.Fatalf("reservation.expired does not describe the reservation: %+v", p)
	}

	// Settled means settled: another pass announces nothing new, and a late
	// attempt to pay is refused before any charge can start.
	expireAll(t, ctx, f)
	if n := len(outboxEvents(t, ctx, held.ReservationID, domain.EventReservationExpired)); n != 1 {
		t.Fatalf("want reservation.expired recorded once, got %d", n)
	}
	if _, err := f.saga.Pay(ctx, held.ReservationID, userID, "too-late"); !errors.Is(err, domain.ErrReservationNotPayable) {
		t.Fatalf("want ErrReservationNotPayable for an expired reservation, got %v", err)
	}
}

// TestExpirerLeavesReservationsReconciliationOwns is the second way to violate
// D8, in the expirer's form.
//
// The payment outcome is unknown and the window closes. Expiring the reservation
// would make it one confirmation refuses, so a charge that did succeed could
// only ever be refunded - and the customer is told their hold expired while
// their money may have moved. The expirer must leave it to reconciliation.
func TestExpirerLeavesReservationsReconciliationOwns(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	_, _, userID, held := hold(t, f)

	f.payment.setMode(paymentTimesOut, domain.PaymentSucceeded)
	if _, err := f.saga.Pay(ctx, held.ReservationID, userID, "unknown"); !errors.Is(err, domain.ErrPaymentOutcomeUnknown) {
		t.Fatalf("want ErrPaymentOutcomeUnknown, got %v", err)
	}
	lapse(t, ctx, held.ReservationID)
	expireAll(t, ctx, f)

	status, pendingSince := reservationState(t, ctx, held.ReservationID)
	if status != string(domain.ReservationPending) || pendingSince == nil {
		t.Fatalf("D8 violated: a reservation reconciliation owns was settled by the expirer (status %s, marked %v)", status, pendingSince)
	}
	if n := len(outboxEvents(t, ctx, held.ReservationID, domain.EventReservationExpired)); n != 0 {
		t.Fatalf("announced the expiry of a reservation whose payment may have succeeded: %d events", n)
	}
}
