//go:build integration

package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/usecase"
)

// Saga tests (ARCHITECTURE.md §6.2, §12). Real Postgres, real Redis,
// fault-injected Payment: the writes these tests assert on are the real ones,
// against the real schema and the real locking queries.
//
// Two stores means two questions, and they are not the same question. "Is the
// seat still held?" is answered by Redis, because that is where a hold lives.
// "Is the seat sold?" is answered by Postgres, because that is where a booking
// lives. A test that asked Postgres the first question would see 'available'
// for a seat somebody is actively checking out with.

// hold seeds a single-seat event and claims it, returning what the saga needs.
func hold(t *testing.T, f *fixture) (eventID, seatID, userID string, held usecase.Hold) {
	t.Helper()
	eventID, seatID = seedOneSeat(t, f)
	userID = uuid.Must(uuid.NewV7()).String()
	held, err := f.holder.HoldSeats(context.Background(), eventID, []string{seatID}, userID, uuid.Must(uuid.NewV7()).String())
	if err != nil {
		t.Fatalf("hold seats: %v", err)
	}
	if held.TotalCents <= 0 {
		t.Fatalf("a hold must carry a price for the saga to charge: got %d", held.TotalCents)
	}
	return eventID, seatID, userID, held
}

// seatStatus reads a seat straight from the database, bypassing every layer
// under test. It answers whether the seat is sold, never whether it is held.
func seatStatus(t *testing.T, ctx context.Context, seatID string) string {
	t.Helper()
	var status string
	if err := testPool.QueryRow(ctx,
		`SELECT status FROM seats WHERE id = $1`, seatID).Scan(&status); err != nil {
		t.Fatalf("read seat: %v", err)
	}
	return status
}

// holdTTLOf reads how long a seat's hold has left. Used to prove the D8
// extension happened, which is otherwise invisible.
func holdTTLOf(t *testing.T, ctx context.Context, f *fixture, eventID, seatID string) time.Duration {
	t.Helper()
	ttl, err := f.redis.TTL(ctx, holdKey(eventID, seatID)).Result()
	if err != nil {
		t.Fatalf("read hold ttl: %v", err)
	}
	return ttl
}

// shrinkHold pulls a hold's expiry in to a few seconds away, so a test can
// watch something push it back out without waiting for a real one to decay.
func shrinkHold(t *testing.T, ctx context.Context, f *fixture, eventID, seatID string) {
	t.Helper()
	ok, err := f.redis.Expire(ctx, holdKey(eventID, seatID), 5*time.Second).Result()
	if err != nil {
		t.Fatalf("shrink hold: %v", err)
	}
	if !ok {
		t.Fatalf("seat %s is not held, so there is no hold to shrink", seatID)
	}
}

// reservationState reads a reservation straight from the database.
func reservationState(t *testing.T, ctx context.Context, reservationID string) (status string, paymentPendingSince *time.Time) {
	t.Helper()
	if err := testPool.QueryRow(ctx,
		`SELECT status, payment_pending_since FROM reservations WHERE id = $1`, reservationID).Scan(&status, &paymentPendingSince); err != nil {
		t.Fatalf("read reservation: %v", err)
	}
	return status, paymentPendingSince
}

// awaitsReconciliation backdates a reservation's payment_pending_since so the
// reconciliation job's grace period has elapsed for it, without the test
// sleeping through one. It is also what keeps these tests independent: the scan
// is global, and only a backdated reservation is eligible.
func awaitsReconciliation(t *testing.T, ctx context.Context, reservationID string) {
	t.Helper()
	tag, err := testPool.Exec(ctx,
		`UPDATE reservations SET payment_pending_since = now() - interval '2 hours'
		 WHERE id = $1 AND payment_pending_since IS NOT NULL`, reservationID)
	if err != nil {
		t.Fatalf("backdate payment_pending_since: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("reservation %s is not marked for reconciliation", reservationID)
	}
}

func activeClaims(t *testing.T, ctx context.Context, seatID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM reservation_seats WHERE seat_id = $1 AND released_at IS NULL`, seatID).Scan(&n); err != nil {
		t.Fatalf("count active claims: %v", err)
	}
	return n
}

// TestPayLeavesReservationPendingOnPaymentTimeout is the D8 test, and the one
// this phase exists to make pass.
//
// The payment call does not answer. The charge may have succeeded. Every
// assertion below is about something NOT happening: the seat is not released,
// the reservation is not failed, the claim is not stamped. Releasing here would
// sell a seat the customer may already have paid for, and it is the single most
// expensive mistake this system can make.
func TestPayLeavesReservationPendingOnPaymentTimeout(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	eventID, seatID, userID, held := hold(t, f)

	// The dangerous variant: the money moved and the answer was lost. Booking
	// cannot tell this apart from a call that never arrived, which is exactly
	// why it may not guess.
	f.payment.setMode(paymentTimesOut, domain.PaymentSucceeded)

	_, err := f.saga.Pay(ctx, held.ReservationID, userID, uuid.Must(uuid.NewV7()).String())
	if !errors.Is(err, domain.ErrPaymentOutcomeUnknown) {
		t.Fatalf("want ErrPaymentOutcomeUnknown, got %v", err)
	}

	if got := holder(t, ctx, f, eventID, seatID); got != held.ReservationID {
		t.Fatalf("D8 violated: the seat is held by %q after a payment timeout, want reservation %s", got, held.ReservationID)
	}
	if status := seatStatus(t, ctx, seatID); status != string(domain.SeatAvailable) {
		t.Fatalf("nothing was sold, so the seat row should be untouched, got %s", status)
	}
	if n := activeClaims(t, ctx, seatID); n != 1 {
		t.Fatalf("D8 violated: want the claim still active, got %d active claims", n)
	}

	resStatus, pendingSince := reservationState(t, ctx, held.ReservationID)
	if resStatus != string(domain.ReservationPending) {
		t.Fatalf("want the reservation still pending, got %s", resStatus)
	}
	if pendingSince == nil {
		t.Fatal("the reservation must be marked for reconciliation, or nothing will ever resolve it")
	}

	// Nobody else can take the seat while the outcome is in doubt.
	if _, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, uuid.Must(uuid.NewV7()).String(), "poacher"); !errors.Is(err, domain.ErrSeatUnavailable) {
		t.Fatalf("want ErrSeatUnavailable while the payment outcome is unknown, got %v", err)
	}
}

// TestPayReleasesSeatsOnDecline is the other half of the pair: a definite no.
//
// Here the outcome IS known, so compensating is not only safe but required -
// somebody else is waiting for the seat (PRD §4.1 step 7).
func TestPayReleasesSeatsOnDecline(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	eventID, seatID, userID, held := hold(t, f)

	f.payment.setMode(paymentDeclines, "")

	conf, err := f.saga.Pay(ctx, held.ReservationID, userID, uuid.Must(uuid.NewV7()).String())
	if !errors.Is(err, domain.ErrPaymentDeclined) {
		t.Fatalf("want ErrPaymentDeclined, got %v", err)
	}
	if conf.Status != domain.ReservationFailed {
		t.Fatalf("want the reservation reported failed, got %s", conf.Status)
	}

	// Released means the Redis key is gone now, not in ten minutes when the TTL
	// would have got there: somebody else is waiting for this seat.
	if got := holder(t, ctx, f, eventID, seatID); got != "" {
		t.Fatalf("want the hold dropped immediately after a decline, still held by %q", got)
	}
	if status := seatStatus(t, ctx, seatID); status != string(domain.SeatAvailable) {
		t.Fatalf("want the seat unsold, got %s", status)
	}
	if n := activeClaims(t, ctx, seatID); n != 0 {
		t.Fatalf("want the claim stamped released so the D13 index frees up, got %d active", n)
	}

	resStatus, pendingSince := reservationState(t, ctx, held.ReservationID)
	if resStatus != string(domain.ReservationFailed) {
		t.Fatalf("want the reservation failed, got %s", resStatus)
	}
	if pendingSince != nil {
		t.Fatalf("a settled reservation must carry no payment doubt, got %s", pendingSince)
	}

	// The released seat is immediately purchasable by someone else.
	if _, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, uuid.Must(uuid.NewV7()).String(), "next-buyer"); err != nil {
		t.Fatalf("re-hold a released seat: %v", err)
	}
}

// TestPayConfirmsOnSuccess covers §6.1: seats booked, reservation confirmed,
// booking recorded, ticket issued - all or none.
func TestPayConfirmsOnSuccess(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	eventID, seatID, userID, held := hold(t, f)

	conf, err := f.saga.Pay(ctx, held.ReservationID, userID, uuid.Must(uuid.NewV7()).String())
	if err != nil {
		t.Fatalf("pay: %v", err)
	}
	if conf.Status != domain.ReservationConfirmed {
		t.Fatalf("want confirmed, got %s", conf.Status)
	}
	if conf.Booking.ID == "" || conf.Booking.PaymentID == "" {
		t.Fatalf("a confirmation must record its booking and the charge that paid for it: %+v", conf.Booking)
	}
	if len(conf.Tickets) != 1 {
		t.Fatalf("want 1 ticket for 1 seat, got %d", len(conf.Tickets))
	}

	if status := seatStatus(t, ctx, seatID); status != string(domain.SeatBooked) {
		t.Fatalf("want the seat booked, got %s", status)
	}
	if n := activeClaims(t, ctx, seatID); n != 1 {
		t.Fatalf("a booked seat keeps its active claim, got %d", n)
	}
	// The claim is stamped confirmed, which is what the D13 index enforces
	// uniqueness on. A booking recorded without it would leave the backstop
	// covering nothing.
	var confirmedClaims int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM reservation_seats WHERE seat_id = $1 AND confirmed_at IS NOT NULL AND released_at IS NULL`,
		seatID).Scan(&confirmedClaims); err != nil {
		t.Fatalf("count confirmed claims: %v", err)
	}
	if confirmedClaims != 1 {
		t.Fatalf("want exactly 1 confirmed claim on a booked seat, got %d", confirmedClaims)
	}
	// And the transient hold is dropped, because Postgres owns this seat now.
	// Leaving the key would make the seat unavailable to a refund buyer for the
	// rest of the window, for no reason anybody could find later.
	if got := holder(t, ctx, f, eventID, seatID); got != "" {
		t.Fatalf("want the hold dropped once the booking committed, still held by %q", got)
	}

	resStatus, _ := reservationState(t, ctx, held.ReservationID)
	if resStatus != string(domain.ReservationConfirmed) {
		t.Fatalf("want the reservation confirmed, got %s", resStatus)
	}
}

// TestPayIsIdempotent proves a replayed payment neither charges twice nor
// issues a second set of tickets (FR-3.7, FR-4.3).
func TestPayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	_, _, userID, held := hold(t, f)

	first, err := f.saga.Pay(ctx, held.ReservationID, userID, "pay-once")
	if err != nil {
		t.Fatalf("first pay: %v", err)
	}
	second, err := f.saga.Pay(ctx, held.ReservationID, userID, "pay-again")
	if err != nil {
		t.Fatalf("replayed pay: %v", err)
	}
	// Deliberately a different client key on the replay: the charge is scoped
	// to the reservation, so two client keys must still be one purchase.
	if first.Booking.ID != second.Booking.ID {
		t.Fatalf("replay produced a second booking: %s then %s", first.Booking.ID, second.Booking.ID)
	}

	var tickets int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE booking_id = $1`, first.Booking.ID).Scan(&tickets); err != nil {
		t.Fatalf("count tickets: %v", err)
	}
	if tickets != 1 {
		t.Fatalf("want 1 ticket after a replay, got %d", tickets)
	}
}

// TestPayRejectsAnotherUsersReservation: a reservation ID is not a capability.
func TestPayRejectsAnotherUsersReservation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	_, _, _, held := hold(t, f)

	_, err := f.saga.Pay(ctx, held.ReservationID, uuid.Must(uuid.NewV7()).String(), "not-mine")
	if !errors.Is(err, domain.ErrNotReservationOwner) {
		t.Fatalf("want ErrNotReservationOwner, got %v", err)
	}
	if status, _ := reservationState(t, ctx, held.ReservationID); status != string(domain.ReservationPending) {
		t.Fatalf("a rejected payment must not disturb the reservation, got %s", status)
	}
}

// TestPayMarksTheReservationBeforeCallingPayment is the crash the saga cannot
// compensate for, because it is the crash of the saga itself.
//
// A Booking that dies while its payment call is in flight writes nothing
// afterwards. What it committed before the call is all that remains, so the
// reconciliation mark has to be part of it: marked only on failure, that crash
// leaves a charge no reconciliation pass will ever look for, and a customer who
// paid for a seat that goes back on sale.
func TestPayMarksTheReservationBeforeCallingPayment(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	_, _, userID, held := hold(t, f)

	var markedDuringCall bool
	f.payment.onCharge = func() {
		_, pendingSince := reservationState(t, ctx, held.ReservationID)
		markedDuringCall = pendingSince != nil
	}

	if _, err := f.saga.Pay(ctx, held.ReservationID, userID, uuid.Must(uuid.NewV7()).String()); err != nil {
		t.Fatalf("pay: %v", err)
	}
	if !markedDuringCall {
		t.Fatal("the reservation was unmarked while its charge was in flight: a Booking crash at that moment orphans the charge")
	}
	// Settling clears the mark, so a confirmed reservation is not mistaken for
	// one in doubt.
	if _, pendingSince := reservationState(t, ctx, held.ReservationID); pendingSince != nil {
		t.Fatalf("a confirmed reservation must carry no payment doubt, got %s", pendingSince)
	}
}

// TestHoldIsKeptAliveWhileThePaymentOutcomeIsUnknown closes the second way D8
// can be violated, and the less obvious one - restated for P3.
//
// In P0 the danger was the sweeper: the saga correctly left a timed-out
// reservation pending, and then a background job released it anyway because
// from its point of view it was just an expired hold. The guard was a WHERE
// clause in one query.
//
// Retiring the sweeper does not retire the danger, it changes who carries it.
// The Redis TTL now does exactly what the sweeper did - releases the seat when
// the window closes - and there is no WHERE clause to add to it, because it is
// not running any SQL. Nobody asks it anything. So the guard has to be an
// action instead of a condition: the moment the outcome becomes unknown, and on
// every reconciliation pass that cannot resolve it, the hold is pushed out.
//
// Deleting that push is a one-line change that breaks nothing visible and sells
// a paid customer's seat ten minutes later. Hence this test.
func TestHoldIsKeptAliveWhileThePaymentOutcomeIsUnknown(t *testing.T) {
	ctx := context.Background()
	const ttl = time.Minute
	f := newFixture(t, ttl)
	eventID, seatID, userID, held := hold(t, f)

	// Pull the hold in to a few seconds from expiry, standing in for a customer
	// who spent most of their checkout window before paying. Done directly
	// rather than by sleeping, so the test is deterministic and quick.
	shrinkHold(t, ctx, f, eventID, seatID)
	if before := holdTTLOf(t, ctx, f, eventID, seatID); before > 10*time.Second {
		t.Fatalf("setup failed: the hold still has %s left", before)
	}

	f.payment.setMode(paymentTimesOut, domain.PaymentSucceeded)
	if _, err := f.saga.Pay(ctx, held.ReservationID, userID, uuid.Must(uuid.NewV7()).String()); !errors.Is(err, domain.ErrPaymentOutcomeUnknown) {
		t.Fatalf("want ErrPaymentOutcomeUnknown, got %v", err)
	}

	if after := holdTTLOf(t, ctx, f, eventID, seatID); after <= 10*time.Second {
		t.Fatalf("D8 violated by the TTL: the hold has %s left and nothing pushed it out; these seats go back on sale under a charge that may have succeeded", after)
	}
	if got := holder(t, ctx, f, eventID, seatID); got != held.ReservationID {
		t.Fatalf("the seat should still be held by %s, got %q", held.ReservationID, got)
	}

	// The same must be true of every pass that fails to resolve. Payment is
	// still unreachable here, so reconciliation learns nothing - and a job that
	// learns nothing must still keep the seats it owns.
	awaitsReconciliation(t, ctx, held.ReservationID)
	shrinkHold(t, ctx, f, eventID, seatID)
	if _, err := f.reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile while unreachable: %v", err)
	}
	if after := holdTTLOf(t, ctx, f, eventID, seatID); after <= 10*time.Second {
		t.Fatalf("D8 violated: a reconciliation pass that could not resolve the reservation let its hold decay to %s", after)
	}

	resStatus, _ := reservationState(t, ctx, held.ReservationID)
	if resStatus != string(domain.ReservationPending) {
		t.Fatalf("want the reservation still pending, got %s", resStatus)
	}
	// And nobody else can take the seat while the outcome is in doubt.
	if _, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, uuid.Must(uuid.NewV7()).String(), "poacher"); !errors.Is(err, domain.ErrSeatUnavailable) {
		t.Fatalf("want ErrSeatUnavailable while the payment outcome is unknown, got %v", err)
	}

	// This test deliberately ends with an unresolved reservation, which is the
	// one kind of leftover that leaks between tests here: the reconciliation
	// scan is global, so a later test's pass would pick this one up and count
	// it. Un-backdate the mark to put it back outside the grace period.
	if _, err := testPool.Exec(ctx,
		`UPDATE reservations SET payment_pending_since = now() WHERE id = $1`, held.ReservationID); err != nil {
		t.Fatalf("un-backdate payment_pending_since: %v", err)
	}
}

// TestReconcileConfirmsAChargeThatSucceeded is the convergence the whole D8
// design rests on: the timeout hid a successful charge, and the customer ends
// up with the seat they paid for.
func TestReconcileConfirmsAChargeThatSucceeded(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	_, seatID, userID, held := hold(t, f)

	f.payment.setMode(paymentTimesOut, domain.PaymentSucceeded) // charged, answer lost
	if _, err := f.saga.Pay(ctx, held.ReservationID, userID, uuid.Must(uuid.NewV7()).String()); !errors.Is(err, domain.ErrPaymentOutcomeUnknown) {
		t.Fatalf("want ErrPaymentOutcomeUnknown, got %v", err)
	}

	awaitsReconciliation(t, ctx, held.ReservationID)

	// Payment becomes answerable again.
	f.payment.reachable()

	resolved, err := f.reconciler.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if resolved != 1 {
		t.Fatalf("want 1 reservation resolved, got %d", resolved)
	}

	if status := seatStatus(t, ctx, seatID); status != string(domain.SeatBooked) {
		t.Fatalf("want the seat booked after reconciliation, got %s", status)
	}
	resStatus, pendingSince := reservationState(t, ctx, held.ReservationID)
	if resStatus != string(domain.ReservationConfirmed) {
		t.Fatalf("want the reservation confirmed, got %s", resStatus)
	}
	if pendingSince != nil {
		t.Fatalf("a resolved reservation must carry no payment doubt, got %s", pendingSince)
	}

	var bookings int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM bookings WHERE reservation_id = $1`, held.ReservationID).Scan(&bookings); err != nil {
		t.Fatalf("count bookings: %v", err)
	}
	if bookings != 1 {
		t.Fatalf("want 1 booking, got %d", bookings)
	}
}

// TestReconcileReleasesWhenNoChargeExists is the other resolution of §6.2 row
// 2: Payment has never seen the key, which is proof no money moved and the only
// evidence that makes releasing safe after a timeout.
func TestReconcileReleasesWhenNoChargeExists(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	eventID, seatID, userID, held := hold(t, f)

	f.payment.setMode(paymentTimesOut, "") // the call never arrived
	if _, err := f.saga.Pay(ctx, held.ReservationID, userID, uuid.Must(uuid.NewV7()).String()); !errors.Is(err, domain.ErrPaymentOutcomeUnknown) {
		t.Fatalf("want ErrPaymentOutcomeUnknown, got %v", err)
	}

	awaitsReconciliation(t, ctx, held.ReservationID)

	// While Payment is still unreachable, reconciliation must change nothing:
	// not being able to ask is not the same as being told no.
	if _, err := f.reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile while unreachable: %v", err)
	}
	if got := holder(t, ctx, f, eventID, seatID); got != held.ReservationID {
		t.Fatalf("D8 violated: reconciliation released a seat without an answer from Payment (held by %q)", got)
	}

	f.payment.reachable()

	if _, err := f.reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := holder(t, ctx, f, eventID, seatID); got != "" {
		t.Fatalf("want the hold dropped once Payment confirmed no charge exists, still held by %q", got)
	}
	if resStatus, _ := reservationState(t, ctx, held.ReservationID); resStatus != string(domain.ReservationFailed) {
		t.Fatalf("want the reservation failed, got %s", resStatus)
	}
	if _, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, uuid.Must(uuid.NewV7()).String(), "next-buyer"); err != nil {
		t.Fatalf("re-hold a reconciled seat: %v", err)
	}

	// "No charge" is only final once calls that started before the release have
	// had time to land. One look after the grace period, with none landed,
	// closes the recheck without moving any money.
	awaitsChargeRecheck(t, ctx, held.ReservationID)
	if _, err := f.reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile the recheck: %v", err)
	}
	if queuedForRecheck(t, ctx, held.ReservationID) {
		t.Fatal("a recheck that found no charge must leave the queue")
	}
}

// awaitsChargeRecheck backdates a released reservation's place in the recheck
// queue past the grace period, and fails the test if it was never queued.
func awaitsChargeRecheck(t *testing.T, ctx context.Context, reservationID string) {
	t.Helper()
	tag, err := testPool.Exec(ctx,
		`UPDATE charge_rechecks SET released_at = now() - interval '2 hours' WHERE reservation_id = $1`, reservationID)
	if err != nil {
		t.Fatalf("backdate charge recheck: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("reservation %s is not queued for a charge recheck", reservationID)
	}
}

func queuedForRecheck(t *testing.T, ctx context.Context, reservationID string) bool {
	t.Helper()
	var n int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM charge_rechecks WHERE reservation_id = $1`, reservationID).Scan(&n); err != nil {
		t.Fatalf("read charge recheck queue: %v", err)
	}
	return n == 1
}

// TestReconcileRefundsAChargeThatLandsAfterRelease is the race "no charge" hides.
//
// Reconciliation asks Payment, hears there is no charge, and releases the seats.
// But a pay retry was already past the fence and on its way to Payment, and it
// takes the money a moment later - for a reservation that is now failed, whose
// seats are back on sale, and which the pending-only scan never looks at again.
// Without a second look that customer has paid for nothing, and nothing in the
// system knows it.
func TestReconcileRefundsAChargeThatLandsAfterRelease(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	_, _, userID, held := hold(t, f)

	f.payment.setMode(paymentTimesOut, "") // the first call never arrived
	if _, err := f.saga.Pay(ctx, held.ReservationID, userID, uuid.Must(uuid.NewV7()).String()); !errors.Is(err, domain.ErrPaymentOutcomeUnknown) {
		t.Fatalf("want ErrPaymentOutcomeUnknown, got %v", err)
	}
	awaitsReconciliation(t, ctx, held.ReservationID)
	f.payment.reachable()

	if _, err := f.reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if status, _ := reservationState(t, ctx, held.ReservationID); status != string(domain.ReservationFailed) {
		t.Fatalf("setup: want the reservation released on no-charge evidence, got %s", status)
	}

	// The retry that was already in flight reaches Payment now, and succeeds.
	late, err := f.payment.Charge(ctx, usecase.ChargeRequest{
		ReservationID:  held.ReservationID,
		UserID:         userID,
		AmountCents:    held.TotalCents,
		IdempotencyKey: usecase.ChargeIdempotencyKey(held.ReservationID),
	})
	if err != nil || late.Status != domain.PaymentSucceeded {
		t.Fatalf("setup: want the late charge to succeed, got %+v, %v", late, err)
	}

	// Inside the grace period a call started before the release may still be
	// running, so the answer is not final and nothing is done yet.
	if _, err := f.reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile inside the grace period: %v", err)
	}
	if _, refunded := f.payment.refundedAmount(late.ChargeID); refunded {
		t.Fatal("the recheck ran before calls started ahead of the release could have landed")
	}

	awaitsChargeRecheck(t, ctx, held.ReservationID)
	if _, err := f.reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile the recheck: %v", err)
	}

	amount, refunded := f.payment.refundedAmount(late.ChargeID)
	if !refunded {
		t.Fatal("FR-5.3 violated: a charge that landed after its reservation was released was never refunded")
	}
	if amount != held.TotalCents {
		t.Fatalf("want the full %d cents refunded, got %d", held.TotalCents, amount)
	}
	if queuedForRecheck(t, ctx, held.ReservationID) {
		t.Fatal("a refunded reservation must leave the recheck queue")
	}
	if status, _ := reservationState(t, ctx, held.ReservationID); status != string(domain.ReservationFailed) {
		t.Fatalf("want the reservation still failed, got %s", status)
	}
}

// TestPaymentMarkFencesOffASettledReservation: the mark is the fence in front of
// every charge. It must refuse a reservation that has settled - that is what
// stops a charge starting after a release, and what makes one recheck enough -
// and must keep the first timestamp on a repeat, or every retry would push
// reconciliation further away.
func TestPaymentMarkFencesOffASettledReservation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)

	_, _, userID, declined := hold(t, f)
	f.payment.setMode(paymentDeclines, "")
	if _, err := f.saga.Pay(ctx, declined.ReservationID, userID, uuid.Must(uuid.NewV7()).String()); !errors.Is(err, domain.ErrPaymentDeclined) {
		t.Fatalf("want ErrPaymentDeclined, got %v", err)
	}
	if pending, err := f.repo.MarkPaymentPending(ctx, declined.ReservationID); err != nil || pending {
		t.Fatalf("the fence would let a charge start for a failed reservation: pending=%v err=%v", pending, err)
	}

	_, _, _, open := hold(t, f)
	if pending, err := f.repo.MarkPaymentPending(ctx, open.ReservationID); err != nil || !pending {
		t.Fatalf("the fence refused a reservation that is still pending: pending=%v err=%v", pending, err)
	}
	_, first := reservationState(t, ctx, open.ReservationID)
	if pending, err := f.repo.MarkPaymentPending(ctx, open.ReservationID); err != nil || !pending {
		t.Fatalf("a repeat mark on a pending reservation must still pass the fence: pending=%v err=%v", pending, err)
	}
	if _, again := reservationState(t, ctx, open.ReservationID); first == nil || again == nil || !again.Equal(*first) {
		t.Fatalf("a repeat mark moved the timestamp from %v to %v", first, again)
	}
}

// TestReconcileRefundsWhenConfirmationIsImpossible is §6.2 row 4 and FR-5.3:
// the money moved, the seat is gone, and the customer must not be left having
// paid for nothing.
func TestReconcileRefundsWhenConfirmationIsImpossible(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	eventID, seatID, userID, held := hold(t, f)

	f.payment.setMode(paymentTimesOut, domain.PaymentSucceeded) // charged, answer lost
	if _, err := f.saga.Pay(ctx, held.ReservationID, userID, uuid.Must(uuid.NewV7()).String()); !errors.Is(err, domain.ErrPaymentOutcomeUnknown) {
		t.Fatalf("want ErrPaymentOutcomeUnknown, got %v", err)
	}

	awaitsReconciliation(t, ctx, held.ReservationID)

	// Force confirmation to be impossible the way P3 actually gets there: the
	// hold lapsed while the charge was in doubt, and somebody else took the
	// seat. Dropping the key is exactly what an expired TTL does.
	if err := f.redis.Del(ctx, holdKey(eventID, seatID)).Err(); err != nil {
		t.Fatalf("drop the hold: %v", err)
	}
	poacher, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, uuid.Must(uuid.NewV7()).String(), "poacher")
	if err != nil {
		t.Fatalf("a lapsed hold must leave the seat claimable: %v", err)
	}

	f.payment.reachable()

	if _, err := f.reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	charge, err := f.payment.GetCharge(ctx, usecase.ChargeIdempotencyKey(held.ReservationID))
	if err != nil {
		t.Fatalf("read charge: %v", err)
	}
	amount, refunded := f.payment.refundedAmount(charge.ChargeID)
	if !refunded {
		t.Fatal("FR-5.3 violated: a paid reservation that cannot be confirmed was not refunded")
	}
	if amount != held.TotalCents {
		t.Fatalf("want the full %d cents refunded, got %d", held.TotalCents, amount)
	}

	resStatus, pendingSince := reservationState(t, ctx, held.ReservationID)
	if resStatus != string(domain.ReservationFailed) {
		t.Fatalf("want the refunded reservation failed, got %s", resStatus)
	}
	if pendingSince != nil {
		t.Fatalf("a resolved reservation must carry no payment doubt, got %s", pendingSince)
	}

	// The invariant, which is what all of this is protecting: the seat belongs
	// to whoever took it after the hold lapsed, and the refunded reservation
	// released nothing of theirs on its way out.
	if got := holder(t, ctx, f, eventID, seatID); got != poacher.ReservationID {
		t.Fatalf("the seat should still be held by the second buyer %s, got %q", poacher.ReservationID, got)
	}
}

// TestPayRejectsAnExpiredReservation covers FR-4.2: the checkout window closed,
// so charging would create a customer who paid for a seat somebody else can buy.
func TestPayRejectsAnExpiredReservation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Millisecond)
	_, _, userID, held := hold(t, f)

	// The checkout window has already closed. Nothing needs to have noticed:
	// expiry is a fact about the clock, and Pay reads it off the reservation.
	_, err := f.saga.Pay(ctx, held.ReservationID, userID, uuid.Must(uuid.NewV7()).String())
	if !errors.Is(err, domain.ErrReservationNotPayable) {
		t.Fatalf("want ErrReservationNotPayable for an expired reservation, got %v", err)
	}
	if f.payment.chargeCalls != 0 {
		t.Fatalf("an expired reservation must not reach Payment at all, got %d charge calls", f.payment.chargeCalls)
	}
}

// TestCompensationSurvivesACancelledRequest is a regression test for a bug the
// unit-level fakes could not have caught, and the live stack did.
//
// The gateway's deadline fires while Booking is waiting on Payment, so the
// request context is already cancelled by the time the saga decides what to do.
// If the compensating write inherits that cancellation it silently does
// nothing: the reservation stays pending with no mark, nothing owns it, its
// hold is never extended, and the TTL releases seats that may already have been
// paid for. D8 violated by way of a context, with no code path that looks wrong.
func TestCompensationSurvivesACancelledRequest(t *testing.T) {
	f := newFixture(t, time.Minute)
	eventID, seatID, userID, held := hold(t, f)

	f.payment.setMode(paymentTimesOut, domain.PaymentSucceeded)
	f.payment.setHangFor(time.Second)

	// The caller's deadline fires while the saga is still waiting on Payment -
	// the gateway giving up, or the customer closing the tab.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := f.saga.Pay(ctx, held.ReservationID, userID, uuid.Must(uuid.NewV7()).String()); !errors.Is(err, domain.ErrPaymentOutcomeUnknown) {
		t.Fatalf("want ErrPaymentOutcomeUnknown, got %v", err)
	}

	check := context.Background()
	resStatus, pendingSince := reservationState(t, check, held.ReservationID)
	if resStatus != string(domain.ReservationPending) {
		t.Fatalf("want the reservation still pending, got %s", resStatus)
	}
	if pendingSince == nil {
		t.Fatal("the reconciliation mark was lost with the request context: these seats are now unowned and the TTL will release them")
	}
	if got := holder(t, check, f, eventID, seatID); got != held.ReservationID {
		t.Fatalf("want the seat still held by %s, got %q", held.ReservationID, got)
	}
	// The hold extension is on the same detached context as the mark, and it is
	// the half that keeps the seat. Losing it to a cancellation would leave a
	// correctly marked reservation whose seats lapse anyway.
	if ttl := holdTTLOf(t, check, f, eventID, seatID); ttl <= 10*time.Second {
		t.Fatalf("D8 violated: the hold was not extended before the request context died, %s left", ttl)
	}
}

// TestReconcileRedrivesAChargePaymentNeverSettled closes the gap that makes
// "leave it pending" a leak rather than a design.
//
// Payment recorded the charge, then its own provider call was interrupted, so
// the row sits unsettled. Nothing else is ever going to finish it: asking again
// read-only would wait forever, nothing else may release the reservation, and
// the customer is left with a hold that never resolves.
// Reconciliation has to push, not just ask.
func TestReconcileRedrivesAChargePaymentNeverSettled(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	_, seatID, userID, held := hold(t, f)

	// Payment kept a charge it could not settle.
	f.payment.setMode(paymentTimesOut, domain.PaymentPending)
	if _, err := f.saga.Pay(ctx, held.ReservationID, userID, uuid.Must(uuid.NewV7()).String()); !errors.Is(err, domain.ErrPaymentOutcomeUnknown) {
		t.Fatalf("want ErrPaymentOutcomeUnknown, got %v", err)
	}
	awaitsReconciliation(t, ctx, held.ReservationID)

	// Payment and its provider both come back.
	f.payment.reachable()

	resolved, err := f.reconciler.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if resolved != 1 {
		t.Fatalf("want the stuck reservation resolved, got %d", resolved)
	}

	if status := seatStatus(t, ctx, seatID); status != string(domain.SeatBooked) {
		t.Fatalf("want the seat booked once the charge settled, got %s", status)
	}
	resStatus, pendingSince := reservationState(t, ctx, held.ReservationID)
	if resStatus != string(domain.ReservationConfirmed) {
		t.Fatalf("want the reservation confirmed, got %s", resStatus)
	}
	if pendingSince != nil {
		t.Fatalf("a resolved reservation must carry no payment doubt, got %s", pendingSince)
	}

	// Re-driving resumes the original charge rather than creating a second one:
	// two calls under one key, one booking.
	if f.payment.chargeCalls != 2 {
		t.Fatalf("want 2 charge calls under one idempotency key, got %d", f.payment.chargeCalls)
	}
	var bookings int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM bookings WHERE reservation_id = $1`, held.ReservationID).Scan(&bookings); err != nil {
		t.Fatalf("count bookings: %v", err)
	}
	if bookings != 1 {
		t.Fatalf("want 1 booking, got %d", bookings)
	}
}
