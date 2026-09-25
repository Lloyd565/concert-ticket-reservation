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

// bookingReplicas is how many independent Booking replicas the holders are
// spread across. P3's exit criterion is that the invariant survives with more
// than one (ARCHITECTURE-ESSENTIALS.md build order).
const bookingReplicas = 2

// TestHoldSeatsConcurrent is the executable proof of the project's one
// invariant: a given seat for a given event is confirmed to at most one
// booking, under any level of concurrency (PRD §1).
//
// Every goroutine requests the same single seat at the same moment - half of
// them through one Booking replica, half through another, each replica with its
// own connection pool and its own Redis client, sharing nothing in this process.
// Exactly one must win; every other must lose with domain.ErrSeatUnavailable - a
// distinguishable conflict, not a 500 and not a deadlock (FR-3.2).
//
// Splitting the callers across replicas is what makes this a P3 test rather
// than a rerun of P0's. An in-process lock, an accidental singleton, a cache
// that happened to be shared - anything that made P0 pass without the storage
// layer actually serialising these callers - fails here, because the two
// replicas have no way to agree except through Redis.
func TestHoldSeatsConcurrent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	replicas := newReplicas(t, bookingReplicas, time.Minute)
	eventID, seatID := seedOneSeat(t, replicas[0])

	type outcome struct {
		reservationID string
		replica       int
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
		replica := i % bookingReplicas
		go func(i, replica int) {
			defer done.Done()
			// Distinct user and idempotency key per caller: this is 64 different
			// people wanting the same seat, not one client retrying.
			userID := uuid.Must(uuid.NewV7()).String()
			key := uuid.Must(uuid.NewV7()).String()
			ready.Done()
			<-start
			held, err := replicas[replica].holder.HoldSeats(ctx, eventID, []string{seatID}, userID, key)
			outcomes[i] = outcome{reservationID: held.ReservationID, replica: replica, err: err}
		}(i, replica)
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
			t.Logf("replica %d won the seat with reservation %s", o.replica, o.reservationID)
		case errors.Is(o.err, domain.ErrBackstopTripped):
			// The invariant survived, but only because the database refused the
			// write. That means the Redis claim did not serialise these callers
			// - the very thing this test exists to prove. Passing on the
			// backstop alone would be passing for the wrong reason.
			t.Errorf("holder %d (replica %d) was stopped by the D13 database backstop, not by the hold store: %v", i, o.replica, o.err)
		case errors.Is(o.err, domain.ErrHoldsUnavailable):
			// Failing closed is correct behaviour, but not here: Redis is up.
			t.Errorf("holder %d (replica %d) could not reach the hold store: %v", i, o.replica, o.err)
		case errors.Is(o.err, domain.ErrSeatUnavailable):
			conflicts++
		default:
			// A pool timeout or a leaked SQL error lands here. Either means the
			// design is wrong, not that the test is flaky.
			t.Errorf("holder %d (replica %d) failed with an unexpected error: %v", i, o.replica, o.err)
		}
	}

	if len(winners) != 1 {
		t.Fatalf("want exactly 1 successful hold, got %d: %v", len(winners), winners)
	}
	if conflicts != concurrentHolders-1 {
		t.Fatalf("want %d seat-unavailable conflicts, got %d", concurrentHolders-1, conflicts)
	}

	// The in-memory tally agrees. Now confirm the stores agree too: a test that
	// only counted return values could be fooled by two writes where the second
	// silently overwrote the first.
	//
	// Redis first, because Redis is the authority on who holds this seat. One
	// key, one value, and it is the winner's reservation.
	if got := holder(t, ctx, replicas[0], eventID, seatID); got != winners[0] {
		t.Fatalf("the seat is held by %q, but the winning reservation was %s", got, winners[0])
	}
	ttl, err := replicas[0].redis.TTL(ctx, holdKey(eventID, seatID)).Result()
	if err != nil {
		t.Fatalf("read hold ttl: %v", err)
	}
	if ttl <= 0 || ttl > time.Minute {
		// A hold with no expiry is a seat leak with extra steps: it is the
		// exact failure the sweeper existed to paper over.
		t.Fatalf("want a hold expiring within the checkout window, got a TTL of %s", ttl)
	}

	// Postgres, meanwhile, still calls the seat available - nothing has been
	// sold. That is not a discrepancy, it is the design: Postgres is the source
	// of truth for confirmed bookings, and there is no booking here.
	seats, err := replicas[0].repo.ListSeatsByEvent(ctx, eventID)
	if err != nil {
		t.Fatalf("list seats: %v", err)
	}
	if len(seats) != 1 {
		t.Fatalf("want 1 seat in the map, got %d", len(seats))
	}
	if seats[0].Status != domain.SeatAvailable {
		t.Fatalf("want the seat still unsold in Postgres, got %s", seats[0].Status)
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

// TestHoldSeatsConcurrentMultiSeat covers all-or-nothing under contention (D6):
// concurrent multi-seat holds, in opposite request orders, across both replicas.
//
// In P0 this was the deadlock test - two transactions locking the same rows in
// different orders is the classic cycle, and sorted acquisition is what
// prevented it. The Lua script removes the possibility rather than managing it:
// a script runs to completion with nothing interleaved, so there is no window in
// which two callers each hold part of what the other wants. What the test now
// proves is the other half of D6 - that a claim which fails partway gives back
// every key it had already taken, leaving no seat stranded for a whole TTL under
// a reservation that never existed.
func TestHoldSeatsConcurrentMultiSeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	replicas := newReplicas(t, bookingReplicas, time.Minute)
	ev, seats, err := replicas[0].seeder.Seed(ctx, seedSpecFor(t, 4))
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
		replica := i % bookingReplicas
		go func(i, replica int, order []string) {
			defer done.Done()
			userID := uuid.Must(uuid.NewV7()).String()
			key := uuid.Must(uuid.NewV7()).String()
			ready.Done()
			<-start
			_, err := replicas[replica].holder.HoldSeats(ctx, ev.ID, order, userID, key)
			errs[i] = err
		}(i, replica, order)
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
			t.Errorf("holder %d was stopped by the D13 database backstop, not by the hold store: %v", i, err)
		case errors.Is(err, domain.ErrHoldsUnavailable):
			t.Errorf("holder %d could not reach the hold store: %v", i, err)
		case errors.Is(err, domain.ErrSeatUnavailable):
		default:
			t.Errorf("holder %d failed with an unexpected error: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("want exactly 1 successful 4-seat hold, got %d", winners)
	}

	// All four seats belong to the one winner, and no fifth key survives from a
	// request that failed halfway through its own claim.
	var winner string
	for _, id := range ids {
		got := holder(t, ctx, replicas[0], ev.ID, id)
		if got == "" {
			t.Fatalf("seat %s is held by nobody; the winning hold was not all-or-nothing", id)
		}
		if winner == "" {
			winner = got
		}
		if got != winner {
			t.Fatalf("seat %s is held by %s but seat %s is held by %s: a partial hold survived", id, got, ids[0], winner)
		}
	}
}
