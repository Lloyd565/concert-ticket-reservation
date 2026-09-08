//go:build integration

package usecase_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
)

// concurrentHolders is the number of goroutines that fight over one seat.
// PRD NFR-1.1 requires at least 50. Never lower it to make a change pass
// (AGENTS.md §9).
const concurrentHolders = 64

// TestHoldSeatsConcurrent is the executable proof of the project's one
// invariant: a given seat for a given event is confirmed to at most one
// booking, under any level of concurrency (PRD §1).
//
// Every goroutine requests the same single seat at the same moment, against a
// real PostgreSQL instance. Exactly one must win; every other must lose with
// domain.ErrSeatUnavailable - a distinguishable conflict, not a 500 and not a
// deadlock (FR-3.2).
func TestHoldSeatsConcurrent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	f := newFixture(t, time.Minute)
	eventID, seatID := seedOneSeat(t, f)

	type outcome struct {
		reservationID string
		err           error
	}
	outcomes := make([]outcome, concurrentHolders)

	// Every goroutine blocks on the same channel so they are released together.
	// Starting them sequentially would let each finish before the next began
	// and the test would exercise nothing.
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(concurrentHolders)
	done.Add(concurrentHolders)

	for i := 0; i < concurrentHolders; i++ {
		go func(i int) {
			defer done.Done()
			// Distinct user and idempotency key per caller: this is 64 different
			// people wanting the same seat, not one client retrying.
			userID := uuid.Must(uuid.NewV7()).String()
			key := uuid.Must(uuid.NewV7()).String()
			ready.Done()
			<-start
			id, _, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, userID, key)
			outcomes[i] = outcome{reservationID: id, err: err}
		}(i)
	}

	ready.Wait()
	close(start)
	done.Wait()

	var (
		winners   []string
		conflicts int
	)
	for i, o := range outcomes {
		switch {
		case o.err == nil:
			winners = append(winners, o.reservationID)
		case errors.Is(o.err, domain.ErrBackstopTripped):
			// The invariant survived, but only because the database refused the
			// write. That means SELECT ... FOR UPDATE did not serialise these
			// callers - the very thing this test exists to prove. Passing on the
			// backstop alone would be passing for the wrong reason.
			t.Errorf("holder %d was stopped by the D13 database backstop, not by the seat lock: %v", i, o.err)
		case errors.Is(o.err, domain.ErrSeatUnavailable):
			conflicts++
		default:
			// A deadlock (40P01), a pool timeout or a leaked SQL error all land
			// here. Any of them means the locking design is wrong, not flaky.
			t.Errorf("holder %d failed with an unexpected error: %v", i, o.err)
		}
	}

	if len(winners) != 1 {
		t.Fatalf("want exactly 1 successful hold, got %d: %v", len(winners), winners)
	}
	if conflicts != concurrentHolders-1 {
		t.Fatalf("want %d seat-unavailable conflicts, got %d", concurrentHolders-1, conflicts)
	}

	// The in-memory tally agrees. Now confirm the database agrees too: a test
	// that only counted return values could be fooled by two writes where the
	// second silently overwrote the first.
	seats, err := f.repo.ListSeatsByEvent(ctx, eventID)
	if err != nil {
		t.Fatalf("list seats: %v", err)
	}
	if len(seats) != 1 {
		t.Fatalf("want 1 seat in the map, got %d", len(seats))
	}
	if seats[0].Status != domain.SeatHeld {
		t.Fatalf("want the seat held, got %s", seats[0].Status)
	}
	if seats[0].HeldBy != winners[0] {
		t.Fatalf("seat is held by %s, but the winning reservation was %s", seats[0].HeldBy, winners[0])
	}

	var activeClaims int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM reservation_seats WHERE seat_id = $1 AND released_at IS NULL`,
		seatID).Scan(&activeClaims); err != nil {
		t.Fatalf("count active claims: %v", err)
	}
	if activeClaims != 1 {
		t.Fatalf("want exactly 1 active claim on the seat, got %d", activeClaims)
	}

	var pending int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM reservations WHERE event_id = $1 AND status = 'pending'`,
		eventID).Scan(&pending); err != nil {
		t.Fatalf("count pending reservations: %v", err)
	}
	if pending != 1 {
		t.Fatalf("want exactly 1 pending reservation, got %d", pending)
	}
}

// TestHoldSeatsConcurrentMultiSeat covers the deadlock scenario D5 exists to
// prevent: concurrent multi-seat holds whose requested order differs. Without
// sorted lock acquisition these transactions wait on each other in a cycle and
// Postgres kills one with a 40P01 - which would surface here as an unexpected
// error rather than a clean conflict.
func TestHoldSeatsConcurrentMultiSeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	f := newFixture(t, time.Minute)
	ev, seats, err := f.seeder.Seed(ctx, seedSpecFor(t, 4))
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}

	ids := []string{seats[0].ID, seats[1].ID, seats[2].ID, seats[3].ID}
	// Two request orders that are exact reverses of each other.
	forward := ids
	reverse := []string{ids[3], ids[2], ids[1], ids[0]}

	errs := make([]error, concurrentHolders)
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(concurrentHolders)
	done.Add(concurrentHolders)

	for i := 0; i < concurrentHolders; i++ {
		order := forward
		if i%2 == 1 {
			order = reverse
		}
		go func(i int, order []string) {
			defer done.Done()
			userID := uuid.Must(uuid.NewV7()).String()
			key := uuid.Must(uuid.NewV7()).String()
			ready.Done()
			<-start
			_, _, err := f.holder.HoldSeats(ctx, ev.ID, order, userID, key)
			errs[i] = err
		}(i, order)
	}

	ready.Wait()
	close(start)
	done.Wait()

	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, domain.ErrBackstopTripped):
			t.Errorf("holder %d was stopped by the D13 database backstop, not by the seat lock: %v", i, err)
		case errors.Is(err, domain.ErrSeatUnavailable):
		default:
			t.Errorf("holder %d failed with an unexpected error (a deadlock would appear here): %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("want exactly 1 successful 4-seat hold, got %d", winners)
	}

	// All-or-nothing across the whole set (D6): every seat belongs to the one
	// winner, or the hold should not have happened at all.
	seatsAfter, err := f.repo.ListSeatsByEvent(ctx, ev.ID)
	if err != nil {
		t.Fatalf("list seats: %v", err)
	}
	for _, s := range seatsAfter {
		if s.Status != domain.SeatHeld {
			t.Fatalf("seat %s/%s is %s, want all four held by the winner", s.Row, s.Number, s.Status)
		}
	}
}
