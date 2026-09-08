//go:build integration

package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
)

func TestHoldSeatsIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	eventID, seatID := seedOneSeat(t, f)

	userID := uuid.Must(uuid.NewV7()).String()
	key := "replay-me"

	first, firstExpiry, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, userID, key)
	if err != nil {
		t.Fatalf("first hold: %v", err)
	}
	// The retry must return the original reservation rather than conflicting
	// with it or claiming a second one (FR-3.7).
	second, secondExpiry, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, userID, key)
	if err != nil {
		t.Fatalf("replayed hold: %v", err)
	}
	if first != second {
		t.Fatalf("replay returned a different reservation: %s then %s", first, second)
	}
	// Tolerance, not equality: the first call returns the deadline Go computed
	// (nanoseconds), the replay returns it read back from a timestamptz column
	// (microseconds). They are the same instant to the precision the database
	// actually stores. What matters is that the replay did not extend the
	// checkout window.
	if d := secondExpiry.Sub(firstExpiry); d < -time.Microsecond || d > time.Microsecond {
		t.Fatalf("replay moved the expiry by %s: %s then %s", d, firstExpiry, secondExpiry)
	}

	var reservations int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM reservations WHERE event_id = $1`, eventID).Scan(&reservations); err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	if reservations != 1 {
		t.Fatalf("want 1 reservation after a replay, got %d", reservations)
	}
}

func TestHoldSeatsIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	ev, seats, err := f.seeder.Seed(ctx, seedSpecFor(t, 3))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	taken, free1, free2 := seats[0].ID, seats[1].ID, seats[2].ID

	if _, _, err := f.holder.HoldSeats(ctx, ev.ID, []string{taken}, uuid.Must(uuid.NewV7()).String(), "first"); err != nil {
		t.Fatalf("hold the contested seat: %v", err)
	}

	// One unavailable seat fails the whole request; the other two must be
	// untouched (D6). A partial hold here would strand seats nobody asked for.
	_, _, err = f.holder.HoldSeats(ctx, ev.ID, []string{free1, taken, free2}, uuid.Must(uuid.NewV7()).String(), "second")
	if !errors.Is(err, domain.ErrSeatUnavailable) {
		t.Fatalf("want ErrSeatUnavailable, got %v", err)
	}

	after, err := f.repo.ListSeatsByEvent(ctx, ev.ID)
	if err != nil {
		t.Fatalf("list seats: %v", err)
	}
	held := 0
	for _, s := range after {
		if s.Status == domain.SeatHeld {
			held++
			if s.ID != taken {
				t.Fatalf("seat %s was held by a request that should have failed entirely", s.ID)
			}
		}
	}
	if held != 1 {
		t.Fatalf("want exactly 1 held seat, got %d", held)
	}
}

func TestHoldSeatsRejectsUnknownSeat(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	eventID, seatID := seedOneSeat(t, f)

	ghost := uuid.Must(uuid.NewV7()).String()
	_, _, err := f.holder.HoldSeats(ctx, eventID, []string{seatID, ghost}, uuid.Must(uuid.NewV7()).String(), "ghost")
	if !errors.Is(err, domain.ErrSeatNotFound) {
		t.Fatalf("want ErrSeatNotFound, got %v", err)
	}

	seats, err := f.repo.ListSeatsByEvent(ctx, eventID)
	if err != nil {
		t.Fatalf("list seats: %v", err)
	}
	if seats[0].Status != domain.SeatAvailable {
		t.Fatalf("the real seat should be untouched, got %s", seats[0].Status)
	}
}

func TestHoldSeatsRejectsMalformedInput(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	eventID, seatID := seedOneSeat(t, f)
	user := uuid.Must(uuid.NewV7()).String()

	if _, _, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, user, ""); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("missing idempotency key: want ErrInvalidInput, got %v", err)
	}
	if _, _, err := f.holder.HoldSeats(ctx, eventID, nil, user, "k1"); !errors.Is(err, domain.ErrNoSeatsRequested) {
		t.Fatalf("no seats: want ErrNoSeatsRequested, got %v", err)
	}
	if _, _, err := f.holder.HoldSeats(ctx, eventID, []string{"not-a-uuid"}, user, "k2"); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("bad seat id: want ErrInvalidInput, got %v", err)
	}
}

// TestSweeperReleasesExpiredHolds proves an abandoned checkout heals itself
// with no user or operator action (PRD §4.1 step 8, NFR-1.2).
func TestSweeperReleasesExpiredHolds(t *testing.T) {
	ctx := context.Background()
	// A TTL that has already elapsed by the time the hold commits, so the test
	// does not sleep out a real checkout window.
	f := newFixture(t, time.Millisecond)
	eventID, seatID := seedOneSeat(t, f)

	reservationID, expiresAt, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, uuid.Must(uuid.NewV7()).String(), "abandoned")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if !expiresAt.After(time.Now().Add(-time.Minute)) {
		t.Fatalf("expiry looks wrong: %s", expiresAt)
	}

	// A second holder cannot take the seat while the hold stands, expired or
	// not: in P0 the sweeper is the only thing that frees it.
	if _, _, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, uuid.Must(uuid.NewV7()).String(), "too-early"); !errors.Is(err, domain.ErrSeatUnavailable) {
		t.Fatalf("want ErrSeatUnavailable before the sweep, got %v", err)
	}

	f.sweeper.SweepOnce(ctx)

	seats, err := f.repo.ListSeatsByEvent(ctx, eventID)
	if err != nil {
		t.Fatalf("list seats: %v", err)
	}
	if seats[0].Status != domain.SeatAvailable {
		t.Fatalf("want the seat available after the sweep, got %s", seats[0].Status)
	}
	if seats[0].HeldBy != "" || seats[0].HeldUntil != nil {
		t.Fatalf("the sweep must clear hold metadata: %+v", seats[0])
	}

	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM reservations WHERE id = $1`, reservationID).Scan(&status); err != nil {
		t.Fatalf("read reservation: %v", err)
	}
	if status != string(domain.ReservationExpired) {
		t.Fatalf("want the reservation expired, got %s", status)
	}

	var activeClaims int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM reservation_seats WHERE seat_id = $1 AND released_at IS NULL`, seatID).Scan(&activeClaims); err != nil {
		t.Fatalf("count active claims: %v", err)
	}
	if activeClaims != 0 {
		t.Fatalf("the sweep must release the claim so the seat can be re-held, got %d active", activeClaims)
	}

	// The freed seat is immediately purchasable by someone else (PRD §4.1 step 7).
	if _, _, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, uuid.Must(uuid.NewV7()).String(), "next-buyer"); err != nil {
		t.Fatalf("re-hold a swept seat: %v", err)
	}
}
