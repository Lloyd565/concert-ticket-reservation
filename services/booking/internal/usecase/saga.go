package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
)

// The reservation saga (ARCHITECTURE.md §6). Booking is the orchestrator: the
// flow lives in one readable place rather than being spread across event
// handlers, because a reviewer has to be able to check it (D7).
//
// Every compensating action in §6.2 is its own named method here, not a branch
// of one catch-all handler. The five rows of that table are five different
// decisions about a customer's money and a customer's seat, and the difference
// between two of them - "declined" and "we do not know" - is the single most
// dangerous distinction in the system. Collapsing them into shared code is
// exactly how it gets lost.
//
//	§6.2 row 1  payment declined                    -> ReleaseDeclined
//	§6.2 row 2  payment call timed out (unknown)    -> LeavePendingForReconciliation
//	§6.2 row 3  hold expired before payment         -> Sweeper (P0, sweeper.go)
//	§6.2 row 4  paid but confirmation impossible    -> RefundUnconfirmable
//	§6.2 row 5  refund on a confirmed booking       -> ReleaseRefunded
//
// The reconciliation job in reconcile.go resolves row 2 into row 1, row 4, or
// the happy path, which is what makes the unknown case converge.

// Saga orchestrates reservation -> payment -> confirmation.
type Saga struct {
	tx           TxManager
	seats        SeatRepository
	reservations ReservationRepository
	bookings     BookingRepository
	payments     PaymentClient
	log          *slog.Logger
}

// NewSaga wires a Saga.
func NewSaga(tx TxManager, seats SeatRepository, reservations ReservationRepository, bookings BookingRepository, payments PaymentClient, log *slog.Logger) *Saga {
	return &Saga{tx: tx, seats: seats, reservations: reservations, bookings: bookings, payments: payments, log: log}
}

// Confirmation is a completed purchase.
type Confirmation struct {
	ReservationID string
	Status        domain.ReservationStatus
	Booking       domain.Booking
	Tickets       []domain.Ticket
	DeclineReason string
}

// ChargeIdempotencyKey is the key a reservation's charge is made under.
//
// It is derived from the reservation rather than chosen by the client, and that
// is load-bearing in two ways. First, the reservation is the correct idempotency
// scope for a charge: two different client keys for one reservation must still
// be one charge, and a client-chosen key cannot guarantee that. Second, it means
// the reconciliation job can ask Payment what happened to a reservation's charge
// without Booking having stored anything at the moment things went wrong - which
// is precisely the moment a write is least likely to have succeeded.
func ChargeIdempotencyKey(reservationID string) string { return "reservation:" + reservationID }

// RefundIdempotencyKey is the key a reservation's automatic refund is made
// under. Derived for the same reason (FR-4.6).
func RefundIdempotencyKey(reservationID string) string { return "refund:reservation:" + reservationID }

// compensationTimeout bounds the writes that follow a payment call.
//
// Those writes run on a context detached from the caller's - see the comment in
// Pay for why - so they need a deadline of their own or a stuck database would
// hold a goroutine forever.
const compensationTimeout = 5 * time.Second

// Pay drives a held reservation to a conclusion (PRD §4.1 step 5).
//
// The ordering is the architecture's hard constraint (D4): the hold committed
// and released its locks in HoldSeats, long before this function runs, and the
// Payment call below is made with no transaction open. A lock held across that
// network hop would turn a 5 ms transaction into a multi-second one under load,
// on the most contended rows in the system.
func (s *Saga) Pay(ctx context.Context, reservationID, userID, idempotencyKey string) (Confirmation, error) {
	if idempotencyKey == "" {
		return Confirmation{}, fmt.Errorf("pay: idempotency key required: %w", domain.ErrInvalidInput)
	}
	res, err := s.reservations.GetReservation(ctx, reservationID)
	if err != nil {
		return Confirmation{}, err
	}
	// A reservation ID is not a capability. The gateway has already established
	// who the caller is; this checks that they are the person who holds these
	// seats.
	if res.UserID != userID {
		return Confirmation{}, fmt.Errorf("reservation %s belongs to another user: %w", reservationID, domain.ErrNotReservationOwner)
	}

	switch {
	case res.Status == domain.ReservationConfirmed:
		// Already paid. Return the booking rather than charging again - the
		// endpoint is idempotent whether or not the client remembers that.
		return s.existingConfirmation(ctx, res)
	case res.Status != domain.ReservationPending:
		return Confirmation{}, fmt.Errorf("reservation %s is %s: %w", reservationID, res.Status, domain.ErrReservationNotPayable)
	case res.Expired(time.Now().UTC()):
		// FR-4.2. The window closed; the sweeper has released these seats or is
		// about to, and charging for seats somebody else can now buy is how a
		// paid-but-unbooked customer is created.
		return Confirmation{}, fmt.Errorf("reservation %s expired at %s: %w", reservationID, res.ExpiresAt.Format(time.RFC3339), domain.ErrReservationNotPayable)
	}

	// A reservation whose outcome is already in doubt is safe to retry: the
	// charge key is derived from the reservation, so this call resumes the
	// existing charge rather than creating a second one, and Payment answers
	// from its own record if it already has one. Racing the reconciliation job
	// is harmless for the same reason - both paths end at the same idempotent
	// confirm.
	if res.PaymentOutcomeUnknown() {
		s.log.InfoContext(ctx, "retrying a payment whose outcome is unknown",
			"reservation_id", res.ID, "unknown_since", res.PaymentPendingSince)
	}

	result, err := s.payments.Charge(ctx, ChargeRequest{
		ReservationID:  res.ID,
		UserID:         res.UserID,
		AmountCents:    res.TotalCents,
		IdempotencyKey: ChargeIdempotencyKey(res.ID),
	})

	// Everything past this line runs on a context detached from the caller's,
	// with a deadline of its own.
	//
	// This is not a nicety. The writes below are what make the payment call
	// safe to have made at all: recording that the outcome is unknown, booking
	// the seats that were paid for, releasing the ones that were not. The
	// moment they are most likely to be skipped is when the request context has
	// already been cancelled - the gateway's deadline fired, or the customer
	// closed the tab - which is exactly the moment a charge is most likely to
	// be in flight. Inheriting that cancellation means the reservation is left
	// pending with nothing marking it for reconciliation, and the sweeper
	// releases seats that may already have been paid for: D8 violated by way of
	// a context, with no code path anywhere that looks wrong.
	//
	// WithoutCancel keeps the values - the correlation ID still travels - and
	// drops only the cancellation.
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensationTimeout)
	defer cancel()

	if err != nil {
		if errors.Is(err, domain.ErrPaymentOutcomeUnknown) {
			// §6.2 row 2. Nothing is released here. Nothing.
			return Confirmation{}, s.LeavePendingForReconciliation(opCtx, res.ID, err)
		}
		// A definite rejection of the request itself: nothing was charged, and
		// the seats stay held for the rest of the checkout window so the
		// customer can retry. The sweeper collects them if they do not.
		return Confirmation{}, err
	}

	return s.dispatch(opCtx, res, result)
}

// dispatch routes a settled Payment answer to the compensating action §6.2
// prescribes for it. It is a switch and nothing else: every arm is a named
// method, so no failure path is defined here by accident.
func (s *Saga) dispatch(ctx context.Context, res domain.Reservation, result domain.PaymentResult) (Confirmation, error) {
	switch result.Status {
	case domain.PaymentSucceeded:
		return s.ConfirmPaid(ctx, res.ID, result)

	case domain.PaymentDeclined, domain.PaymentFailed:
		// §6.2 row 1.
		if err := s.ReleaseDeclined(ctx, res.ID, result.DeclineReason); err != nil {
			return Confirmation{}, err
		}
		return Confirmation{
			ReservationID: res.ID,
			Status:        domain.ReservationFailed,
			DeclineReason: result.DeclineReason,
		}, fmt.Errorf("reservation %s: %s: %w", res.ID, result.DeclineReason, domain.ErrPaymentDeclined)

	default:
		// Payment answered, but with "I do not know yet" - it has a charge under
		// this key whose provider call has not settled. Same rule as a timeout:
		// change nothing, and let reconciliation resolve it (§6.2 row 2).
		return Confirmation{}, s.LeavePendingForReconciliation(ctx, res.ID,
			fmt.Errorf("payment reports the charge is still %s", result.Status))
	}
}

// ConfirmPaid is the happy path, and the resolution reconciliation reaches when
// it finds a charge that succeeded (§6.2 rows 4 and the §6.1 happy path).
//
// Seats go held -> booked, the reservation goes pending -> confirmed, a booking
// is recorded and its tickets are issued - all in one transaction, because a
// booking without tickets and tickets without a booking are both states nothing
// downstream should have to handle.
//
// It is idempotent. A reservation that is already confirmed returns its existing
// booking, so a client retry, a reconciliation pass and the live saga can all
// call it for the same reservation without issuing two sets of tickets.
//
// It returns domain.ErrConfirmUnrecoverable when the reservation can no longer
// become a booking. That is not a generic failure: it is the signal that the
// customer has paid for something that cannot be delivered, and the only correct
// answer is RefundUnconfirmable.
func (s *Saga) ConfirmPaid(ctx context.Context, reservationID string, charge domain.PaymentResult) (Confirmation, error) {
	var out Confirmation

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		// Lock the reservation first, everywhere, before the seats it owns.
		// Consistent ordering across the confirm, release and expiry paths is
		// what stops them forming a waiting cycle (D5 applied above seat level).
		res, err := s.reservations.LockReservation(ctx, reservationID)
		if err != nil {
			return err
		}

		// Re-read under the lock, because the status this caller remembered
		// from before the payment call is stale by definition. In between, the
		// sweeper may have expired this reservation, or another pass may have
		// confirmed it.
		if res.Status == domain.ReservationConfirmed {
			booking, err := s.bookings.GetBookingByReservation(ctx, reservationID)
			if err != nil {
				return err
			}
			tickets, err := s.bookings.ListTicketsByBooking(ctx, booking.ID)
			if err != nil {
				return err
			}
			out = Confirmation{ReservationID: res.ID, Status: res.Status, Booking: booking, Tickets: tickets}
			return nil
		}
		if res.Status != domain.ReservationPending {
			return fmt.Errorf("reservation %s is %s and cannot be confirmed: %w", res.ID, res.Status, domain.ErrConfirmUnrecoverable)
		}

		seats, err := s.seats.LockSeatsByReservation(ctx, reservationID)
		if err != nil {
			return err
		}
		if len(seats) == 0 {
			// The claims were released - the sweeper got here first, or a
			// compensation did. The seats may already belong to somebody else.
			return fmt.Errorf("reservation %s holds no seats: %w", res.ID, domain.ErrConfirmUnrecoverable)
		}
		// Run the transition through the domain state machine rather than
		// trusting the UPDATE: the rule about which states may become booked
		// lives in one place (AGENTS.md §4), and an illegal transition is a
		// typed error here instead of a silently unaffected row.
		for i := range seats {
			if err := seats[i].Confirm(); err != nil {
				return fmt.Errorf("confirm seat %s (%v): %w", seats[i].ID, err, domain.ErrConfirmUnrecoverable)
			}
		}
		n, err := s.seats.MarkSeatsBooked(ctx, reservationID)
		if err != nil {
			return err
		}
		if n != int64(len(seats)) {
			// Tripwire: the rows were verified held under lock, so a short
			// count means the lock did not do its job.
			return fmt.Errorf("booked %d of %d locked seats: %w", n, len(seats), domain.ErrConfirmUnrecoverable)
		}

		ok, err := s.reservations.SetReservationStatus(ctx, res.ID, domain.ReservationPending, domain.ReservationConfirmed)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("reservation %s moved out of pending under our lock: %w", res.ID, domain.ErrConfirmUnrecoverable)
		}

		now := time.Now().UTC()
		booking := domain.Booking{
			ID:            uuid.Must(uuid.NewV7()).String(),
			ReservationID: res.ID,
			UserID:        res.UserID,
			PaymentID:     charge.ChargeID,
			Status:        domain.BookingConfirmed,
			ConfirmedAt:   now,
		}
		if err := s.bookings.CreateBooking(ctx, booking); err != nil {
			return err
		}

		tickets := make([]domain.Ticket, 0, len(seats))
		for _, seat := range seats {
			tickets = append(tickets, domain.Ticket{
				ID:        uuid.Must(uuid.NewV7()).String(),
				BookingID: booking.ID,
				SeatID:    seat.ID,
				// ponytail: a UUID is the code. A real QR payload is signed so
				// the door can verify it offline; that belongs with the
				// scanning story, which does not exist yet.
				QRCode:   "TKT-" + uuid.Must(uuid.NewV7()).String(),
				IssuedAt: now,
			})
		}
		if err := s.bookings.CreateTickets(ctx, tickets); err != nil {
			return err
		}

		out = Confirmation{ReservationID: res.ID, Status: domain.ReservationConfirmed, Booking: booking, Tickets: tickets}
		return nil
	})
	if err != nil {
		return Confirmation{}, err
	}

	s.log.InfoContext(ctx, "reservation confirmed",
		"reservation_id", out.ReservationID, "booking_id", out.Booking.ID, "seats", len(out.Tickets), "charge_id", charge.ChargeID)
	return out, nil
}

// ReleaseDeclined is §6.2 row 1: the provider definitively refused.
//
// The outcome is known, so compensating is safe and required: the seats go back
// to available immediately - somebody else is waiting for them (PRD §4.1 step 7)
// - and the reservation goes pending -> failed.
//
// This function must never be reached from a timeout. If it ever is, a customer
// whose charge actually succeeded loses their seat. That is why the timeout has
// its own function rather than sharing this one with a flag.
func (s *Saga) ReleaseDeclined(ctx context.Context, reservationID, reason string) error {
	released, err := s.releasePendingReservation(ctx, reservationID)
	if err != nil {
		return err
	}
	s.log.InfoContext(ctx, "payment declined; seats released",
		"reservation_id", reservationID, "seats_released", released, "reason", reason)
	return nil
}

// ReleaseNeverCharged resolves §6.2 row 2 in the customer's favour: the payment
// outcome was unknown, reconciliation asked Payment, and Payment has no charge
// under this reservation's key at all.
//
// That answer - and only that answer - makes releasing safe, because it is
// proof no money moved. It is a separate function from ReleaseDeclined despite
// doing the same writes, because the two are reached from opposite kinds of
// evidence and they should be separately greppable when someone is working out
// why a seat was freed.
func (s *Saga) ReleaseNeverCharged(ctx context.Context, reservationID string) error {
	released, err := s.releasePendingReservation(ctx, reservationID)
	if err != nil {
		return err
	}
	s.log.InfoContext(ctx, "reconciliation found no charge; seats released",
		"reservation_id", reservationID, "seats_released", released)
	return nil
}

// LeavePendingForReconciliation is §6.2 row 2, and it is the most important
// function in this file precisely because of what it does not do.
//
// The payment call did not complete. The charge may have succeeded. Releasing
// the seats here would be the single worst bug this system can have: the
// customer is charged, and the seat they paid for is sold to somebody else
// while they are still looking at a spinner. So no seat is touched, no
// reservation status changes, and the reservation is marked as belonging to the
// reconciliation job - which also stops the sweeper from releasing it when the
// checkout window closes, because the sweeper would otherwise commit exactly
// the same mistake ten minutes later.
//
// Unknown is not failed (D8). The cost of being wrong in this direction is a
// seat held longer than necessary. The cost of being wrong in the other
// direction is a double-sold seat and a refund dispute.
func (s *Saga) LeavePendingForReconciliation(ctx context.Context, reservationID string, cause error) error {
	marked, err := s.reservations.MarkPaymentPending(ctx, reservationID)
	if err != nil {
		// Even this failing changes nothing about the seats. The reservation
		// stays pending; the worst case is that the sweeper expires it and
		// releases seats for a charge that may have succeeded - which is why
		// the error is loud.
		s.log.ErrorContext(ctx, "could not mark reservation for reconciliation; its seats are now at risk of being swept",
			"reservation_id", reservationID, "error", err)
		return err
	}
	s.log.WarnContext(ctx, "payment outcome unknown; reservation left pending for reconciliation",
		"reservation_id", reservationID, "newly_marked", marked, "cause", cause)
	return fmt.Errorf("reservation %s: %w", reservationID, domain.ErrPaymentOutcomeUnknown)
}

// RefundUnconfirmable is §6.2 row 4: the money moved, but the reservation can
// never become a booking - its window closed and its seats are gone.
//
// FR-5.3 says the system converges and never leaves a paid-but-unbooked
// customer. Confirmation has already been shown to be impossible, so the only
// convergence left is to give the money back.
//
// The refund happens BEFORE the reservation is settled, and outside the
// transaction (D4). If the refund does not complete, nothing is written and the
// next reconciliation pass tries again: a reservation marked failed while its
// refund silently never happened is a customer who paid for nothing and has no
// record saying so.
func (s *Saga) RefundUnconfirmable(ctx context.Context, res domain.Reservation, charge domain.PaymentResult) error {
	if charge.ChargeID == "" {
		return fmt.Errorf("refund reservation %s: no charge to refund against: %w", res.ID, domain.ErrInvalidInput)
	}
	amount := charge.AmountCents
	if amount == 0 {
		amount = res.TotalCents
	}
	if err := s.payments.Refund(ctx, charge.ChargeID, amount, RefundIdempotencyKey(res.ID)); err != nil {
		s.log.ErrorContext(ctx, "automatic refund did not complete; reservation left for the next pass",
			"reservation_id", res.ID, "charge_id", charge.ChargeID, "error", err)
		return err
	}

	// Refunded, so the money is back. The reservation ends as failed rather
	// than refunded: refunded is a state a *confirmed* booking reaches
	// (FR-5.1), and this one never became a booking at all.
	if _, err := s.releasePendingReservation(ctx, res.ID); err != nil {
		return err
	}
	s.log.WarnContext(ctx, "paid reservation could not be confirmed; refunded automatically",
		"reservation_id", res.ID, "charge_id", charge.ChargeID, "amount_cents", amount)
	return nil
}

// ReleaseRefunded is §6.2 row 5: a confirmed booking has been refunded, so its
// seats go back on sale and the booking is marked refunded.
//
// Payment refunds first and this runs afterwards, never the other way round:
// releasing seats before the refund is confirmed frees them for a refund that
// might then fail, leaving a customer with no seat and no money.
//
// P2 reaches it only from RefundUnconfirmable's sibling case - a confirmed
// booking. Its own trigger, the refund.completed event, arrives with the broker
// in P4; the action half is built now because row 4 is unfinished without it.
func (s *Saga) ReleaseRefunded(ctx context.Context, reservationID string) error {
	var released int64

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		res, err := s.reservations.LockReservation(ctx, reservationID)
		if err != nil {
			return err
		}
		if res.Status == domain.ReservationRefunded {
			return nil // Already done. Idempotent by re-read, not by hope.
		}
		if res.Status != domain.ReservationConfirmed {
			return fmt.Errorf("reservation %s is %s, not confirmed: %w", res.ID, res.Status, domain.ErrInvalidTransition)
		}

		booking, err := s.bookings.GetBookingByReservation(ctx, reservationID)
		if err != nil {
			return err
		}
		seats, err := s.seats.LockSeatsByReservation(ctx, reservationID)
		if err != nil {
			return err
		}
		for i := range seats {
			if err := seats[i].Refund(); err != nil {
				return fmt.Errorf("refund seat %s: %w", seats[i].ID, err)
			}
		}
		if released, err = s.seats.ReleaseBookedSeats(ctx, reservationID); err != nil {
			return err
		}
		if released != int64(len(seats)) {
			return fmt.Errorf("released %d of %d booked seats: %w", released, len(seats), domain.ErrInvalidTransition)
		}

		if _, err := s.bookings.SetBookingStatus(ctx, booking.ID, domain.BookingConfirmed, domain.BookingRefunded); err != nil {
			return err
		}
		_, err = s.reservations.SetReservationStatus(ctx, res.ID, domain.ReservationConfirmed, domain.ReservationRefunded)
		return err
	})
	if err != nil {
		return err
	}
	s.log.InfoContext(ctx, "booking refunded; seats released", "reservation_id", reservationID, "seats_released", released)
	return nil
}

// releasePendingReservation is the write shared by the two release paths above.
//
// It is unexported on purpose. The decision about whether releasing is safe is
// the dangerous part and belongs to the named callers, each of which documents
// the evidence that justifies it; this only performs the writes once that
// decision has been made.
func (s *Saga) releasePendingReservation(ctx context.Context, reservationID string) (int64, error) {
	var released int64

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		res, err := s.reservations.LockReservation(ctx, reservationID)
		if err != nil {
			return err
		}
		switch res.Status {
		case domain.ReservationPending:
			// The expected case; fall through to the writes below.
		case domain.ReservationFailed, domain.ReservationExpired:
			// Already settled and already released. Nothing to undo.
			return nil
		default:
			return fmt.Errorf("reservation %s is %s and cannot be released: %w", res.ID, res.Status, domain.ErrInvalidTransition)
		}

		seats, err := s.seats.LockSeatsByReservation(ctx, reservationID)
		if err != nil {
			return err
		}
		for i := range seats {
			if err := seats[i].Release(); err != nil {
				return fmt.Errorf("release seat %s: %w", seats[i].ID, err)
			}
		}
		if released, err = s.seats.ReleaseHeldSeats(ctx, reservationID); err != nil {
			return err
		}
		ok, err := s.reservations.SetReservationStatus(ctx, res.ID, domain.ReservationPending, domain.ReservationFailed)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("reservation %s moved out of pending under our lock: %w", res.ID, domain.ErrInvalidTransition)
		}
		return nil
	})
	return released, err
}

// existingConfirmation returns the booking a reservation was already confirmed
// into, so a replayed Pay is answered rather than re-charged.
func (s *Saga) existingConfirmation(ctx context.Context, res domain.Reservation) (Confirmation, error) {
	booking, err := s.bookings.GetBookingByReservation(ctx, res.ID)
	if err != nil {
		return Confirmation{}, err
	}
	tickets, err := s.bookings.ListTicketsByBooking(ctx, booking.ID)
	if err != nil {
		return Confirmation{}, err
	}
	return Confirmation{ReservationID: res.ID, Status: res.Status, Booking: booking, Tickets: tickets}, nil
}
