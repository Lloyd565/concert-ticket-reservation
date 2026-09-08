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

// Saga tests (ARCHITECTURE.md §6.2, §12). Real Postgres, fault-injected
// Payment: the seat-state writes these tests assert on are the real ones,
// against the real schema and the real locking queries.

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
// under test.
func seatStatus(t *testing.T, ctx context.Context, seatID string) (status string, heldBy *string) {
	t.Helper()
	if err := testPool.QueryRow(ctx,
		`SELECT status, held_by_reservation::text FROM seats WHERE id = $1`, seatID).Scan(&status, &heldBy); err != nil {
		t.Fatalf("read seat: %v", err)
	}
	return status, heldBy
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

	status, heldBy := seatStatus(t, ctx, seatID)
	if status != string(domain.SeatHeld) {
		t.Fatalf("D8 violated: the seat is %s after a payment timeout, want it still held", status)
	}
	if heldBy == nil || *heldBy != held.ReservationID {
		t.Fatalf("D8 violated: the seat is no longer held by reservation %s (held_by=%v)", held.ReservationID, heldBy)
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

	status, heldBy := seatStatus(t, ctx, seatID)
	if status != string(domain.SeatAvailable) {
		t.Fatalf("want the seat released to available after a decline, got %s", status)
	}
	if heldBy != nil {
		t.Fatalf("a released seat must carry no holder, got %v", heldBy)
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

	// The released seat is immediately purchasable by someone else. Freeing the
	// row without freeing the claim would pass every check above and still fail
	// here, on the D13 backstop.
	if _, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, uuid.Must(uuid.NewV7()).String(), "next-buyer"); err != nil {
		t.Fatalf("re-hold a released seat: %v", err)
	}
}

// TestPayConfirmsOnSuccess covers §6.1: seats booked, reservation confirmed,
// booking recorded, ticket issued - all or none.
func TestPayConfirmsOnSuccess(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	_, seatID, userID, held := hold(t, f)

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

	status, heldBy := seatStatus(t, ctx, seatID)
	if status != string(domain.SeatBooked) {
		t.Fatalf("want the seat booked, got %s", status)
	}
	if heldBy != nil {
		// A booked seat is claimed permanently, not until a deadline. The
		// database CHECK says the same thing.
		t.Fatalf("a booked seat must carry no hold metadata, got held_by=%v", heldBy)
	}
	if n := activeClaims(t, ctx, seatID); n != 1 {
		t.Fatalf("a booked seat keeps its active claim, got %d", n)
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

// TestSweeperLeavesReservationsAwaitingReconciliation closes the second way D8
// can be violated, and the less obvious one.
//
// The saga correctly leaves a timed-out reservation pending - and then the P0
// sweeper comes along when the checkout window closes and releases it anyway,
// because from its point of view it is just an expired hold. The guard is a
// WHERE clause, which is exactly the kind of thing that gets dropped in a later
// edit, so it gets a test that fails loudly if it ever is.
func TestSweeperLeavesReservationsAwaitingReconciliation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	_, seatID, userID, held := hold(t, f)

	f.payment.setMode(paymentTimesOut, domain.PaymentSucceeded)
	if _, err := f.saga.Pay(ctx, held.ReservationID, userID, uuid.Must(uuid.NewV7()).String()); !errors.Is(err, domain.ErrPaymentOutcomeUnknown) {
		t.Fatalf("want ErrPaymentOutcomeUnknown, got %v", err)
	}

	// Close the checkout window, which is the sweeper's whole trigger. Done by
	// hand rather than by holding for a millisecond, because Pay refuses an
	// already-expired reservation (FR-4.2) and this test needs the order the
	// other way round: charged first, expired second.
	if _, err := testPool.Exec(ctx,
		`UPDATE reservations SET expires_at = now() - interval '1 minute' WHERE id = $1`, held.ReservationID); err != nil {
		t.Fatalf("expire reservation: %v", err)
	}

	f.sweeper.SweepOnce(ctx)

	status, _ := seatStatus(t, ctx, seatID)
	if status != string(domain.SeatHeld) {
		t.Fatalf("D8 violated by the sweeper: the seat is %s, want it still held while its payment outcome is unknown", status)
	}
	resStatus, _ := reservationState(t, ctx, held.ReservationID)
	if resStatus != string(domain.ReservationPending) {
		t.Fatalf("D8 violated by the sweeper: the reservation is %s, want it still pending", resStatus)
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

	if status, _ := seatStatus(t, ctx, seatID); status != string(domain.SeatBooked) {
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
	if status, _ := seatStatus(t, ctx, seatID); status != string(domain.SeatHeld) {
		t.Fatalf("D8 violated: reconciliation released a seat without an answer from Payment (seat is %s)", status)
	}

	f.payment.reachable()

	if _, err := f.reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if status, _ := seatStatus(t, ctx, seatID); status != string(domain.SeatAvailable) {
		t.Fatalf("want the seat released once Payment confirmed no charge exists, got %s", status)
	}
	if resStatus, _ := reservationState(t, ctx, held.ReservationID); resStatus != string(domain.ReservationFailed) {
		t.Fatalf("want the reservation failed, got %s", resStatus)
	}
	if _, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, uuid.Must(uuid.NewV7()).String(), "next-buyer"); err != nil {
		t.Fatalf("re-hold a reconciled seat: %v", err)
	}
}

// TestReconcileRefundsWhenConfirmationIsImpossible is §6.2 row 4 and FR-5.3:
// the money moved, the seat is gone, and the customer must not be left having
// paid for nothing.
func TestReconcileRefundsWhenConfirmationIsImpossible(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	_, seatID, userID, held := hold(t, f)

	f.payment.setMode(paymentTimesOut, domain.PaymentSucceeded) // charged, answer lost
	if _, err := f.saga.Pay(ctx, held.ReservationID, userID, uuid.Must(uuid.NewV7()).String()); !errors.Is(err, domain.ErrPaymentOutcomeUnknown) {
		t.Fatalf("want ErrPaymentOutcomeUnknown, got %v", err)
	}

	awaitsReconciliation(t, ctx, held.ReservationID)

	// Force confirmation to be impossible: strip the reservation of its claim
	// the way an operator intervention or a bug elsewhere would. The reservation
	// is still pending and still marked, but it holds nothing to book.
	if _, err := testPool.Exec(ctx,
		`UPDATE reservation_seats SET released_at = now() WHERE reservation_id = $1`, held.ReservationID); err != nil {
		t.Fatalf("strip claim: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE seats SET status = 'available', held_by_reservation = NULL, held_until = NULL WHERE id = $1`, seatID); err != nil {
		t.Fatalf("free seat: %v", err)
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
}

// TestPayRejectsAnExpiredReservation covers FR-4.2: the checkout window closed,
// so charging would create a customer who paid for a seat somebody else can buy.
func TestPayRejectsAnExpiredReservation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Millisecond)
	_, _, userID, held := hold(t, f)

	// The hold TTL has already elapsed; the sweeper has not necessarily run.
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
// nothing: the reservation stays pending with no mark, nothing owns it, and the
// sweeper releases seats that may already have been paid for. D8 violated by
// way of a context, with no code path that looks wrong.
func TestCompensationSurvivesACancelledRequest(t *testing.T) {
	f := newFixture(t, time.Minute)
	_, seatID, userID, held := hold(t, f)

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
		t.Fatal("the reconciliation mark was lost with the request context: these seats are now unowned and the sweeper will release them")
	}
	if status, _ := seatStatus(t, check, seatID); status != string(domain.SeatHeld) {
		t.Fatalf("want the seat still held, got %s", status)
	}

	// And the mark does its job: the sweeper leaves it alone.
	if _, err := testPool.Exec(check,
		`UPDATE reservations SET expires_at = now() - interval '1 minute' WHERE id = $1`, held.ReservationID); err != nil {
		t.Fatalf("expire reservation: %v", err)
	}
	f.sweeper.SweepOnce(check)
	if status, _ := seatStatus(t, check, seatID); status != string(domain.SeatHeld) {
		t.Fatalf("D8 violated: the sweeper released a seat whose payment outcome is unknown (seat is %s)", status)
	}
}

// TestReconcileRedrivesAChargePaymentNeverSettled closes the gap that makes
// "leave it pending" a leak rather than a design.
//
// Payment recorded the charge, then its own provider call was interrupted, so
// the row sits unsettled. Nothing else is ever going to finish it: asking again
// read-only would wait forever, the sweeper is forbidden to touch the
// reservation, and the customer is left with a hold that never resolves.
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

	if status, _ := seatStatus(t, ctx, seatID); status != string(domain.SeatBooked) {
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
