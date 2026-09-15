package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
)

// DefaultHoldTTL is the checkout window (PRD §4.1 step 4). In P3 it is a real
// TTL on a Redis key, not a timestamp somebody has to come back and check.
const DefaultHoldTTL = 10 * time.Minute

// Hold is a successful claim on a set of seats.
type Hold struct {
	ReservationID string
	// ExpiresAt is when the checkout window closes. Returned explicitly so the
	// client can show a countdown rather than guess (FR-3.3).
	ExpiresAt time.Time
	// TotalCents is what the reservation is worth, fixed at claim time. It is
	// the amount the saga charges.
	TotalCents int64
}

// Holder claims seats on behalf of a user. It owns the single most contended
// operation in the system.
type Holder struct {
	tx           TxManager
	holds        HoldStore
	seats        SeatRepository
	reservations ReservationRepository
	ttl          time.Duration
	log          *slog.Logger
}

// NewHolder wires a Holder. A zero ttl falls back to DefaultHoldTTL.
func NewHolder(tx TxManager, holds HoldStore, seats SeatRepository, reservations ReservationRepository, ttl time.Duration, log *slog.Logger) *Holder {
	if ttl <= 0 {
		ttl = DefaultHoldTTL
	}
	return &Holder{tx: tx, holds: holds, seats: seats, reservations: reservations, ttl: ttl, log: log}
}

// HoldSeats atomically claims every requested seat for userID, or claims none.
//
// It returns the new reservation, its checkout deadline and its price. Losers
// of a race for a seat get domain.ErrSeatUnavailable, which the transport layer
// maps to 409 rather than 500 (FR-3.2).
//
// idempotencyKey is required (AGENTS.md §2 rule 10, FR-3.7): replaying a
// request with a key that has already been used returns the original
// reservation instead of claiming a second set of seats.
//
// Nothing external is called from inside the transaction. The claim is taken
// before the transaction opens and the writes commit before this returns, so
// the Payment call the saga makes next happens strictly afterwards (D4).
func (h *Holder) HoldSeats(ctx context.Context, eventID string, seatIDs []string, userID, idempotencyKey string) (Hold, error) {
	if idempotencyKey == "" {
		return Hold{}, fmt.Errorf("hold seats: idempotency key required: %w", domain.ErrInvalidInput)
	}
	if _, err := uuid.Parse(eventID); err != nil {
		// Parsed here rather than only in the repository, because the Redis key
		// is built from it before any query runs: a malformed event ID must not
		// become a hold key for an event that cannot exist.
		return Hold{}, fmt.Errorf("hold seats: invalid event id %q (%v): %w", eventID, err, domain.ErrInvalidInput)
	}
	requested, err := canonicalSeatIDs(seatIDs)
	if err != nil {
		return Hold{}, fmt.Errorf("hold seats: %w", err)
	}

	// Idempotency first: a replay must not take a claim it does not need, and
	// must not create a second reservation. This is a read, so it needs no
	// transaction of its own.
	switch existing, err := h.reservations.FindReservationByIdempotencyKey(ctx, idempotencyKey); {
	case err == nil:
		return Hold{ReservationID: existing.ID, ExpiresAt: existing.ExpiresAt, TotalCents: existing.TotalCents}, nil
	case !errors.Is(err, domain.ErrReservationNotFound):
		return Hold{}, err
	}

	now := time.Now().UTC()
	res := domain.Reservation{
		ID:      uuid.Must(uuid.NewV7()).String(),
		UserID:  userID,
		EventID: eventID,
		Status:  domain.ReservationPending,
		SeatIDs: requested,
		// A mirror of the Redis TTL, not a second authority for it. It is what
		// the client counts down against and what Pay checks before charging;
		// the key itself is what actually stops a second holder, and it expires
		// on its own whether or not anybody reads this column.
		ExpiresAt:      now.Add(h.ttl),
		IdempotencyKey: idempotencyKey,
		CreatedAt:      now,
	}

	// ---- The claim --------------------------------------------------------
	//
	// One round trip, one script, all seats or none (D6). SET NX is the lock:
	// the key either did not exist and is now ours, or it did and we lost. There
	// is no window between checking and taking because there is no check - the
	// taking IS the check, decided inside Redis where nothing else can interleave.
	//
	// Fail closed (D12): if this returns ErrHoldsUnavailable nothing below runs.
	// There is deliberately no fallback path to Postgres locking here. A hold
	// taken without the lock is a double-booking waiting for its second caller,
	// and "Redis was down" is not a reason a customer would accept for being
	// sold a seat somebody else is sitting in.
	if err := h.holds.Claim(ctx, eventID, requested, res.ID, h.ttl); err != nil {
		return Hold{}, fmt.Errorf("hold seats: %w", err)
	}
	// Every failure from here on must give the claim back. The TTL would do it
	// eventually, but "eventually" is ten minutes of a seat nobody can buy.
	claimed := true
	defer func() {
		if claimed {
			return
		}
		h.releaseClaim(ctx, res)
	}()

	// ---- The catalog ------------------------------------------------------
	//
	// Read AFTER the claim, never before, and the order is the whole guard.
	// Confirmation commits 'booked' to Postgres and only then deletes the Redis
	// key, so if we won a key that a confirmation had just released, that
	// confirmation's write is already visible here and this reads 'booked'.
	// Reading first and claiming second would reopen precisely that window.
	catalog, err := h.seats.GetSeats(ctx, eventID, requested)
	if err != nil {
		claimed = false
		return Hold{}, err
	}
	if len(catalog) != len(requested) {
		claimed = false
		return Hold{}, fmt.Errorf("%d of %d seats exist for event %s: %w", len(catalog), len(requested), eventID, domain.ErrSeatNotFound)
	}
	for i := range catalog {
		if !catalog[i].Available() {
			// Sold, not held: a held seat does not appear here at all, its key
			// stopped us above. This is the seat that was bought while we were
			// deciding, or before the event's seat map was re-read.
			claimed = false
			return Hold{}, fmt.Errorf("seat %s is %s: %w", catalog[i].ID, catalog[i].Status, domain.ErrSeatUnavailable)
		}
		// The amount is fixed here and stored on the reservation. Re-summing it
		// at payment time would let a price change between the hold and the
		// charge move the total out from under the customer.
		res.TotalCents += catalog[i].PriceCents
	}

	// ---- The record -------------------------------------------------------
	//
	// The seats are already ours; this is what makes the reservation legible to
	// the saga, to reconciliation and to the customer. The two rows commit
	// together: a reservation whose seats nobody recorded cannot be confirmed.
	err = h.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := h.reservations.CreateReservation(ctx, res); err != nil {
			return err
		}
		return h.reservations.AttachSeats(ctx, res.ID, requested)
	})
	if err != nil {
		claimed = false
		return Hold{}, err
	}

	return Hold{ReservationID: res.ID, ExpiresAt: res.ExpiresAt, TotalCents: res.TotalCents}, nil
}

// releaseClaim hands back a claim whose reservation was never recorded.
//
// Best-effort by design: it runs on a context detached from the caller's, so a
// cancelled request still gives the seats back, and a failure is logged rather
// than returned because the caller is already failing for a better reason. The
// TTL is the backstop - the cost of this not working is one checkout window of
// an unbuyable seat, never a lost seat.
func (h *Holder) releaseClaim(ctx context.Context, res domain.Reservation) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if _, err := h.holds.Release(ctx, res.EventID, res.SeatIDs, res.ID); err != nil {
		h.log.ErrorContext(ctx, "could not release a claim whose hold failed; it will expire with its TTL",
			"reservation_id", res.ID, "seats", len(res.SeatIDs), "error", err)
	}
}

// canonicalSeatIDs validates, de-duplicates and sorts the requested seat IDs.
//
// Canonical lowercase form matters twice over: Postgres compares uuid values as
// bytes, so sorting canonical text here yields the same order as ORDER BY id
// there, and the Redis key is built from this exact string - two spellings of
// one seat ID would be two different keys and therefore no lock at all.
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
	// Sorted so every code path claims contended seats in the same sequence
	// (D5), and so the Redis script's key order matches the order the seat IDs
	// are reported in.
	sort.Strings(out)
	return out, nil
}
