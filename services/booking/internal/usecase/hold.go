package usecase

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
)

// DefaultHoldTTL is the checkout window (PRD §4.1 step 4).
const DefaultHoldTTL = 10 * time.Minute

// Holder claims seats on behalf of a user. It owns the single most contended
// operation in the system.
type Holder struct {
	tx           TxManager
	seats        SeatRepository
	reservations ReservationRepository
	ttl          time.Duration
}

// NewHolder wires a Holder. A zero ttl falls back to DefaultHoldTTL.
func NewHolder(tx TxManager, seats SeatRepository, reservations ReservationRepository, ttl time.Duration) *Holder {
	if ttl <= 0 {
		ttl = DefaultHoldTTL
	}
	return &Holder{tx: tx, seats: seats, reservations: reservations, ttl: ttl}
}

// HoldSeats atomically claims every requested seat for userID, or claims none.
//
// It returns the new reservation's ID and the instant its checkout window
// closes. Losers of a race for a seat get domain.ErrSeatUnavailable, which the
// transport layer maps to 409 rather than 500 (FR-3.2).
//
// idempotencyKey is required (AGENTS.md §2 rule 10, FR-3.7): replaying a
// request with a key that has already been used returns the original
// reservation instead of claiming a second set of seats.
//
// Nothing external is called from inside the transaction. The claim commits and
// the locks drop before this function returns, so the Payment call added in P2
// happens strictly afterwards (D4).
func (h *Holder) HoldSeats(ctx context.Context, eventID string, seatIDs []string, userID, idempotencyKey string) (string, time.Time, error) {
	if idempotencyKey == "" {
		return "", time.Time{}, fmt.Errorf("hold seats: idempotency key required: %w", domain.ErrInvalidInput)
	}
	requested, err := canonicalSeatIDs(seatIDs)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("hold seats: %w", err)
	}

	var (
		reservationID string
		expiresAt     time.Time
	)

	err = h.tx.WithinTx(ctx, func(ctx context.Context) error {
		// Idempotency first: a replay must not take locks it does not need,
		// and must not create a second reservation.
		switch existing, err := h.reservations.FindReservationByIdempotencyKey(ctx, idempotencyKey); {
		case err == nil:
			reservationID, expiresAt = existing.ID, existing.ExpiresAt
			return nil
		case !errors.Is(err, domain.ErrReservationNotFound):
			return err
		}

		// ---- The check ----------------------------------------------------
		//
		// SELECT ... FOR UPDATE takes a write lock on each seat row and holds
		// it until this transaction ends. A concurrent hold for any of these
		// seats blocks on that lock instead of reading a stale 'available', so
		// the check-then-act window that would let two callers both believe a
		// seat is free simply does not exist. The loser resumes only after the
		// winner commits, and then reads 'held'.
		//
		// The repository query orders by seat ID (D5): every transaction takes
		// contended rows in the same sequence, so two multi-seat holds can
		// never each be waiting on a row the other already holds. No cycle, no
		// deadlock. requested is sorted for the same reason - the Go-side order
		// and the SQL-side order agree.
		locked, err := h.seats.LockSeatsForUpdate(ctx, eventID, requested)
		if err != nil {
			return err
		}
		if len(locked) != len(requested) {
			return fmt.Errorf("%d of %d seats exist for event %s: %w", len(locked), len(requested), eventID, domain.ErrSeatNotFound)
		}
		// All-or-nothing (D6): every seat is inspected before anything is
		// written, so a partial hold cannot be left behind by an early return.
		for i := range locked {
			if !locked[i].Available() {
				return fmt.Errorf("seat %s is %s: %w", locked[i].ID, locked[i].Status, domain.ErrSeatUnavailable)
			}
		}

		// ---- The act ------------------------------------------------------
		//
		// Still inside the same transaction, so these writes are covered by the
		// locks taken above.
		now := time.Now().UTC()
		res := domain.Reservation{
			ID:             uuid.Must(uuid.NewV7()).String(),
			UserID:         userID,
			EventID:        eventID,
			Status:         domain.ReservationPending,
			SeatIDs:        requested,
			ExpiresAt:      now.Add(h.ttl),
			IdempotencyKey: idempotencyKey,
			CreatedAt:      now,
		}
		// The reservation row exists before any seat points at it: seats
		// .held_by_reservation is a foreign key into it.
		if err := h.reservations.CreateReservation(ctx, res); err != nil {
			return err
		}
		if err := h.reservations.AttachSeats(ctx, res.ID, requested); err != nil {
			return err
		}

		// Run the transition through the domain state machine rather than
		// trusting the UPDATE. The rule about which states may become held
		// lives in one place (AGENTS.md §4), and an illegal transition is a
		// typed error here instead of a silently ignored row.
		for i := range locked {
			if err := locked[i].Hold(res.ID, res.ExpiresAt); err != nil {
				return fmt.Errorf("hold seat %s: %w", locked[i].ID, err)
			}
		}
		n, err := h.seats.MarkSeatsHeld(ctx, requested, res.ID, res.ExpiresAt)
		if err != nil {
			return err
		}
		// Tripwire, not a check: the rows were verified available under lock, so
		// a short count means the lock did not do its job. Fail the whole
		// transaction rather than record a partial hold.
		if n != int64(len(requested)) {
			return fmt.Errorf("held %d of %d locked seats: %w", n, len(requested), domain.ErrSeatUnavailable)
		}

		reservationID, expiresAt = res.ID, res.ExpiresAt
		return nil
	})
	if err != nil {
		return "", time.Time{}, err
	}
	// Committed. Locks are released and the caller is free to make external
	// calls - which is where Payment enters in P2.
	return reservationID, expiresAt, nil
}

// canonicalSeatIDs validates, de-duplicates and sorts the requested seat IDs.
//
// Canonical lowercase form matters: Postgres compares uuid values as bytes, so
// sorting canonical text here yields the same order as ORDER BY id there. If the
// two orders disagreed, the deterministic lock ordering would be a fiction.
func canonicalSeatIDs(seatIDs []string) ([]string, error) {
	if len(seatIDs) == 0 {
		return nil, domain.ErrNoSeatsRequested
	}
	seen := make(map[string]struct{}, len(seatIDs))
	out := make([]string, 0, len(seatIDs))
	for _, raw := range seatIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid seat id %q (%v): %w", raw, err, domain.ErrInvalidInput)
		}
		s := id.String()
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}
