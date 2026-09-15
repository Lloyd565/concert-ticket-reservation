package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
)

// The saga half of the repository (P2). Kept in its own file rather than grown
// onto repo.go so the P0 locking path stays readable on its own: the code that
// closes the double-booking race is the code most likely to be read under
// pressure, and it should not need scrolling past confirmation plumbing.

// LockReservation takes the reservation's write lock and returns its state as
// of the lock. Must be called inside a transaction.
//
// Every saga path locks the reservation before the seats it owns. That ordering
// is not incidental: it is what stops a confirm and a release for the same
// reservation from deadlocking against each other, and the confirm and refund
// paths take their seat locks in the same order below.
func (r *Repo) LockReservation(ctx context.Context, id string) (domain.Reservation, error) {
	resID, err := uuid.Parse(id)
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("parse reservation id %q: %w", id, domain.ErrInvalidInput)
	}
	row, err := r.q(ctx).LockReservationForUpdate(ctx, resID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Reservation{}, domain.ErrReservationNotFound
	}
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("lock reservation: %w", err)
	}
	return r.withSeatIDs(ctx, domain.Reservation{
		ID:                  row.ID.String(),
		UserID:              row.UserID.String(),
		EventID:             row.EventID.String(),
		Status:              domain.ReservationStatus(row.Status),
		TotalCents:          row.TotalCents,
		ExpiresAt:           row.ExpiresAt,
		IdempotencyKey:      row.IdempotencyKey,
		CreatedAt:           row.CreatedAt,
		PaymentPendingSince: row.PaymentPendingSince,
	})
}

// GetReservation reads a reservation without locking it.
func (r *Repo) GetReservation(ctx context.Context, id string) (domain.Reservation, error) {
	resID, err := uuid.Parse(id)
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("parse reservation id %q: %w", id, domain.ErrInvalidInput)
	}
	row, err := r.q(ctx).GetReservationByID(ctx, resID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Reservation{}, domain.ErrReservationNotFound
	}
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("get reservation: %w", err)
	}
	return r.withSeatIDs(ctx, domain.Reservation{
		ID:                  row.ID.String(),
		UserID:              row.UserID.String(),
		EventID:             row.EventID.String(),
		Status:              domain.ReservationStatus(row.Status),
		TotalCents:          row.TotalCents,
		ExpiresAt:           row.ExpiresAt,
		IdempotencyKey:      row.IdempotencyKey,
		CreatedAt:           row.CreatedAt,
		PaymentPendingSince: row.PaymentPendingSince,
	})
}

// SetReservationStatus moves a reservation between two states, reporting
// whether the row was still in the from state.
func (r *Repo) SetReservationStatus(ctx context.Context, id string, from, to domain.ReservationStatus) (bool, error) {
	resID, err := uuid.Parse(id)
	if err != nil {
		return false, fmt.Errorf("parse reservation id %q: %w", id, domain.ErrInvalidInput)
	}
	n, err := r.q(ctx).SetReservationStatus(ctx, SetReservationStatusParams{
		NewStatus:     string(to),
		ID:            resID,
		CurrentStatus: string(from),
	})
	if err != nil {
		return false, fmt.Errorf("set reservation status: %w", err)
	}
	return n == 1, nil
}

// MarkPaymentPending hands the reservation to the reconciliation job (D8),
// reporting whether it was still pending - the fence in front of every charge.
func (r *Repo) MarkPaymentPending(ctx context.Context, id string) (bool, error) {
	resID, err := uuid.Parse(id)
	if err != nil {
		return false, fmt.Errorf("parse reservation id %q: %w", id, domain.ErrInvalidInput)
	}
	n, err := r.q(ctx).MarkPaymentPending(ctx, resID)
	if err != nil {
		return false, fmt.Errorf("mark payment pending: %w", err)
	}
	return n == 1, nil
}

// ListReservationsAwaitingReconciliation returns pending reservations whose
// payment outcome has been unknown since before olderThan.
func (r *Repo) ListReservationsAwaitingReconciliation(ctx context.Context, olderThan time.Time, limit int) ([]domain.Reservation, error) {
	cutoff := olderThan
	rows, err := r.q(ctx).ListReservationsAwaitingReconciliation(ctx, ListReservationsAwaitingReconciliationParams{
		OlderThan: &cutoff,
		RowLimit:  int32(limit), //nolint:gosec // limit is a small configured constant
	})
	if err != nil {
		return nil, fmt.Errorf("list reservations awaiting reconciliation: %w", err)
	}
	out := make([]domain.Reservation, 0, len(rows))
	for _, row := range rows {
		res, err := r.withSeatIDs(ctx, domain.Reservation{
			ID:                  row.ID.String(),
			UserID:              row.UserID.String(),
			EventID:             row.EventID.String(),
			Status:              domain.ReservationStatus(row.Status),
			TotalCents:          row.TotalCents,
			ExpiresAt:           row.ExpiresAt,
			IdempotencyKey:      row.IdempotencyKey,
			CreatedAt:           row.CreatedAt,
			PaymentPendingSince: row.PaymentPendingSince,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}

// QueueChargeRecheck queues a reservation released on "no charge" evidence for
// one more look at its charge key. Must run in the release's transaction.
func (r *Repo) QueueChargeRecheck(ctx context.Context, reservationID string) error {
	resID, err := uuid.Parse(reservationID)
	if err != nil {
		return fmt.Errorf("parse reservation id %q: %w", reservationID, domain.ErrInvalidInput)
	}
	if err := r.q(ctx).QueueChargeRecheck(ctx, resID); err != nil {
		return fmt.Errorf("queue charge recheck: %w", err)
	}
	return nil
}

// ListReservationsAwaitingChargeRecheck returns queued reservations released
// before olderThan.
func (r *Repo) ListReservationsAwaitingChargeRecheck(ctx context.Context, olderThan time.Time, limit int) ([]domain.Reservation, error) {
	rows, err := r.q(ctx).ListReservationsAwaitingChargeRecheck(ctx, ListReservationsAwaitingChargeRecheckParams{
		OlderThan: olderThan,
		RowLimit:  int32(limit), //nolint:gosec // limit is a small configured constant
	})
	if err != nil {
		return nil, fmt.Errorf("list reservations awaiting charge recheck: %w", err)
	}
	out := make([]domain.Reservation, 0, len(rows))
	for _, row := range rows {
		res, err := r.withSeatIDs(ctx, domain.Reservation{
			ID:                  row.ID.String(),
			UserID:              row.UserID.String(),
			EventID:             row.EventID.String(),
			Status:              domain.ReservationStatus(row.Status),
			TotalCents:          row.TotalCents,
			ExpiresAt:           row.ExpiresAt,
			IdempotencyKey:      row.IdempotencyKey,
			CreatedAt:           row.CreatedAt,
			PaymentPendingSince: row.PaymentPendingSince,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}

// ClearChargeRecheck removes a reservation from the recheck queue.
func (r *Repo) ClearChargeRecheck(ctx context.Context, reservationID string) error {
	resID, err := uuid.Parse(reservationID)
	if err != nil {
		return fmt.Errorf("parse reservation id %q: %w", reservationID, domain.ErrInvalidInput)
	}
	if err := r.q(ctx).ClearChargeRecheck(ctx, resID); err != nil {
		return fmt.Errorf("clear charge recheck: %w", err)
	}
	return nil
}

// LockSeatsByReservation locks the seats a reservation actively claims, in
// sorted seat-ID order (D5). Must be called inside a transaction.
func (r *Repo) LockSeatsByReservation(ctx context.Context, reservationID string) ([]domain.Seat, error) {
	resID, err := uuid.Parse(reservationID)
	if err != nil {
		return nil, fmt.Errorf("parse reservation id %q: %w", reservationID, domain.ErrInvalidInput)
	}
	rows, err := r.q(ctx).LockSeatsByReservation(ctx, resID)
	if err != nil {
		return nil, fmt.Errorf("lock seats by reservation: %w", err)
	}
	seats := make([]domain.Seat, 0, len(rows))
	for _, row := range rows {
		seats = append(seats, toSeat(row.ID, row.EventID, row.Section, row.Row, row.Number,
			row.Status, row.PriceCents))
	}
	return seats, nil
}

// MarkSeatsBooked flips a reservation's available seats to booked and stamps
// its claims confirmed.
func (r *Repo) MarkSeatsBooked(ctx context.Context, reservationID string) (int64, error) {
	resID, err := uuid.Parse(reservationID)
	if err != nil {
		return 0, fmt.Errorf("parse reservation id %q: %w", reservationID, domain.ErrInvalidInput)
	}
	n, err := r.q(ctx).MarkSeatsBooked(ctx, resID)
	if isUniqueViolation(err, "one_confirmed_claim_per_seat") {
		// The D13 backstop fired: two reservations reached confirmation for the
		// same seat. Reported as unavailable so the caller compensates - it
		// wraps ErrSeatUnavailable, and the saga refunds a charge it cannot
		// deliver - but reaching this line means both the Redis claim and the
		// FOR UPDATE above it failed to serialise the two. Alert on it.
		return 0, fmt.Errorf("book seats for reservation %s: %w", reservationID, domain.ErrBackstopTripped)
	}
	if err != nil {
		return 0, fmt.Errorf("mark seats booked: %w", err)
	}
	return n, nil
}

// ReleaseClaims settles an unconfirmed reservation's claim rows. It touches no
// seat: in P3 an unconfirmed claim never owned one - the Redis key did, and the
// caller drops that separately.
func (r *Repo) ReleaseClaims(ctx context.Context, reservationID string) (int64, error) {
	resID, err := uuid.Parse(reservationID)
	if err != nil {
		return 0, fmt.Errorf("parse reservation id %q: %w", reservationID, domain.ErrInvalidInput)
	}
	n, err := r.q(ctx).ReleaseReservationClaims(ctx, resID)
	if err != nil {
		return 0, fmt.Errorf("release reservation claims: %w", err)
	}
	return n, nil
}

// ReleaseBookedSeats returns a reservation's booked seats to available.
func (r *Repo) ReleaseBookedSeats(ctx context.Context, reservationID string) (int64, error) {
	resID, err := uuid.Parse(reservationID)
	if err != nil {
		return 0, fmt.Errorf("parse reservation id %q: %w", reservationID, domain.ErrInvalidInput)
	}
	n, err := r.q(ctx).ReleaseBookedSeats(ctx, resID)
	if err != nil {
		return 0, fmt.Errorf("release booked seats: %w", err)
	}
	return n, nil
}

// CreateBooking records a completed purchase, mapping the reservation_id
// UNIQUE violation to a domain error so a duplicate confirm is recognised
// rather than reported as an internal failure.
func (r *Repo) CreateBooking(ctx context.Context, b domain.Booking) error {
	id, err := uuid.Parse(b.ID)
	if err != nil {
		return fmt.Errorf("parse booking id %q: %w", b.ID, domain.ErrInvalidInput)
	}
	resID, err := uuid.Parse(b.ReservationID)
	if err != nil {
		return fmt.Errorf("parse reservation id %q: %w", b.ReservationID, domain.ErrInvalidInput)
	}
	userID, err := uuid.Parse(b.UserID)
	if err != nil {
		return fmt.Errorf("parse user id %q: %w", b.UserID, domain.ErrInvalidInput)
	}
	paymentID, err := uuid.Parse(b.PaymentID)
	if err != nil {
		return fmt.Errorf("parse payment id %q: %w", b.PaymentID, domain.ErrInvalidInput)
	}
	err = r.q(ctx).CreateBooking(ctx, CreateBookingParams{
		ID:            id,
		ReservationID: resID,
		UserID:        userID,
		PaymentID:     paymentID,
		Status:        string(b.Status),
		ConfirmedAt:   b.ConfirmedAt,
	})
	if isUniqueViolation(err, "bookings_reservation_id_key") {
		return domain.ErrBookingExists
	}
	if err != nil {
		return fmt.Errorf("create booking: %w", err)
	}
	return nil
}

// GetBookingByReservation returns the booking a reservation was confirmed into.
func (r *Repo) GetBookingByReservation(ctx context.Context, reservationID string) (domain.Booking, error) {
	resID, err := uuid.Parse(reservationID)
	if err != nil {
		return domain.Booking{}, fmt.Errorf("parse reservation id %q: %w", reservationID, domain.ErrInvalidInput)
	}
	row, err := r.q(ctx).GetBookingByReservation(ctx, resID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Booking{}, domain.ErrBookingNotFound
	}
	if err != nil {
		return domain.Booking{}, fmt.Errorf("get booking by reservation: %w", err)
	}
	return domain.Booking{
		ID:            row.ID.String(),
		ReservationID: row.ReservationID.String(),
		UserID:        row.UserID.String(),
		PaymentID:     row.PaymentID.String(),
		Status:        domain.BookingStatus(row.Status),
		ConfirmedAt:   row.ConfirmedAt,
	}, nil
}

// SetBookingStatus moves a booking between two states, reporting whether the
// row was still in the from state.
func (r *Repo) SetBookingStatus(ctx context.Context, id string, from, to domain.BookingStatus) (bool, error) {
	bookingID, err := uuid.Parse(id)
	if err != nil {
		return false, fmt.Errorf("parse booking id %q: %w", id, domain.ErrInvalidInput)
	}
	n, err := r.q(ctx).SetBookingStatus(ctx, SetBookingStatusParams{
		NewStatus:     string(to),
		ID:            bookingID,
		CurrentStatus: string(from),
	})
	if err != nil {
		return false, fmt.Errorf("set booking status: %w", err)
	}
	return n == 1, nil
}

// CreateTickets issues the booking's tickets in one round trip.
func (r *Repo) CreateTickets(ctx context.Context, tickets []domain.Ticket) error {
	params := make([]CreateTicketsParams, 0, len(tickets))
	for _, t := range tickets {
		id, err := uuid.Parse(t.ID)
		if err != nil {
			return fmt.Errorf("parse ticket id %q: %w", t.ID, domain.ErrInvalidInput)
		}
		bookingID, err := uuid.Parse(t.BookingID)
		if err != nil {
			return fmt.Errorf("parse booking id %q: %w", t.BookingID, domain.ErrInvalidInput)
		}
		seatID, err := uuid.Parse(t.SeatID)
		if err != nil {
			return fmt.Errorf("parse seat id %q: %w", t.SeatID, domain.ErrInvalidInput)
		}
		params = append(params, CreateTicketsParams{ID: id, BookingID: bookingID, SeatID: seatID, QrCode: t.QRCode})
	}
	if _, err := r.q(ctx).CreateTickets(ctx, params); err != nil {
		return fmt.Errorf("create tickets: %w", err)
	}
	return nil
}

// ListTicketsByBooking returns a booking's tickets.
func (r *Repo) ListTicketsByBooking(ctx context.Context, bookingID string) ([]domain.Ticket, error) {
	id, err := uuid.Parse(bookingID)
	if err != nil {
		return nil, fmt.Errorf("parse booking id %q: %w", bookingID, domain.ErrInvalidInput)
	}
	rows, err := r.q(ctx).ListTicketsByBooking(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list tickets: %w", err)
	}
	out := make([]domain.Ticket, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.Ticket{
			ID:        row.ID.String(),
			BookingID: row.BookingID.String(),
			SeatID:    row.SeatID.String(),
			QRCode:    row.QrCode,
			IssuedAt:  row.IssuedAt,
		})
	}
	return out, nil
}

// withSeatIDs fills in a reservation's active seat claims.
func (r *Repo) withSeatIDs(ctx context.Context, res domain.Reservation) (domain.Reservation, error) {
	id, err := uuid.Parse(res.ID)
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("parse reservation id %q: %w", res.ID, domain.ErrInvalidInput)
	}
	seatIDs, err := r.q(ctx).GetReservationSeatIDs(ctx, id)
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("load reservation seats: %w", err)
	}
	for _, seatID := range seatIDs {
		res.SeatIDs = append(res.SeatIDs, seatID.String())
	}
	return res, nil
}
