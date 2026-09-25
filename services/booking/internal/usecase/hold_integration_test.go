//go:build integration

package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
)

func TestHoldSeatsIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	eventID, seatID := seedOneSeat(t, f)

	userID := uuid.Must(uuid.NewV7()).String()
	key := "replay-me"

	first, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, userID, key)
	if err != nil {
		t.Fatalf("first hold: %v", err)
	}
	// The retry must return the original reservation rather than conflicting
	// with it or claiming a second one (FR-3.7). Note that the seat is already
	// held by the first call: a replay that reached the claim would collide
	// with itself, so this also proves the idempotency check runs first.
	second, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, userID, key)
	if err != nil {
		t.Fatalf("replayed hold: %v", err)
	}
	if first.ReservationID != second.ReservationID {
		t.Fatalf("replay returned a different reservation: %s then %s", first.ReservationID, second.ReservationID)
	}
	if first.TotalCents != second.TotalCents {
		t.Fatalf("replay returned a different total: %d then %d", first.TotalCents, second.TotalCents)
	}
	// Tolerance, not equality: the first call returns the deadline Go computed
	// (nanoseconds), the replay returns it read back from a timestamptz column
	// (microseconds). They are the same instant to the precision the database
	// actually stores. What matters is that the replay did not extend the
	// checkout window.
	if d := second.ExpiresAt.Sub(first.ExpiresAt); d < -time.Microsecond || d > time.Microsecond {
		t.Fatalf("replay moved the expiry by %s: %s then %s", d, first.ExpiresAt, second.ExpiresAt)
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

	first, err := f.holder.HoldSeats(ctx, ev.ID, []string{taken}, uuid.Must(uuid.NewV7()).String(), "first")
	if err != nil {
		t.Fatalf("hold the contested seat: %v", err)
	}

	// One unavailable seat fails the whole request, and the two the script had
	// already claimed on the way to finding out are given back (D6). A partial
	// hold here would strand seats nobody asked for for a whole TTL.
	_, err = f.holder.HoldSeats(ctx, ev.ID, []string{free1, taken, free2}, uuid.Must(uuid.NewV7()).String(), "second")
	if !errors.Is(err, domain.ErrSeatUnavailable) {
		t.Fatalf("want ErrSeatUnavailable, got %v", err)
	}

	if got := holder(t, ctx, f, ev.ID, taken); got != first.ReservationID {
		t.Fatalf("the contested seat should still be held by %s, got %q", first.ReservationID, got)
	}
	for _, id := range []string{free1, free2} {
		if got := holder(t, ctx, f, ev.ID, id); got != "" {
			t.Fatalf("seat %s is held by %s after a request that should have failed entirely", id, got)
		}
	}
	// And they are genuinely free, not merely key-less.
	if _, err := f.holder.HoldSeats(ctx, ev.ID, []string{free1, free2}, uuid.Must(uuid.NewV7()).String(), "third"); err != nil {
		t.Fatalf("the rolled-back seats should be immediately claimable: %v", err)
	}
}

func TestHoldSeatsRejectsUnknownSeat(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	eventID, seatID := seedOneSeat(t, f)

	ghost := uuid.Must(uuid.NewV7()).String()
	_, err := f.holder.HoldSeats(ctx, eventID, []string{seatID, ghost}, uuid.Must(uuid.NewV7()).String(), "ghost")
	if !errors.Is(err, domain.ErrSeatNotFound) {
		t.Fatalf("want ErrSeatNotFound, got %v", err)
	}

	// The claim is taken before the catalog is read, so a request naming a seat
	// that does not exist has already locked the one that does. It has to give
	// it back, or a typo would take a real seat off sale for ten minutes.
	if got := holder(t, ctx, f, eventID, seatID); got != "" {
		t.Fatalf("the real seat is still held by %s after a failed request", got)
	}
	if _, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, uuid.Must(uuid.NewV7()).String(), "next"); err != nil {
		t.Fatalf("the real seat should be untouched and claimable: %v", err)
	}
}

func TestHoldSeatsRejectsMalformedInput(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	eventID, seatID := seedOneSeat(t, f)
	user := uuid.Must(uuid.NewV7()).String()

	if _, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, user, ""); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("missing idempotency key: want ErrInvalidInput, got %v", err)
	}
	if _, err := f.holder.HoldSeats(ctx, eventID, nil, user, "k1"); !errors.Is(err, domain.ErrNoSeatsRequested) {
		t.Fatalf("no seats: want ErrNoSeatsRequested, got %v", err)
	}
	if _, err := f.holder.HoldSeats(ctx, eventID, []string{"not-a-uuid"}, user, "k2"); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("bad seat id: want ErrInvalidInput, got %v", err)
	}
	if _, err := f.holder.HoldSeats(ctx, "not-a-uuid", []string{seatID}, user, "k3"); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("bad event id: want ErrInvalidInput, got %v", err)
	}
}

// TestAbandonedHoldExpiresWithItsTTL is P3's exit criterion for expiry: an
// abandoned checkout heals itself with no user, no operator and - the part that
// is new - no sweeper (PRD §4.1 step 8, NFR-1.2).
//
// There is no sweeper to call here, and that is the assertion. In P0 this test
// had to invoke one; the seat came back only because a background job noticed.
// Now nothing notices. Redis drops the key when its TTL runs out, on its own
// clock, whether or not any Booking replica is even running - which is why the
// service can be replicated at all.
func TestAbandonedHoldExpiresWithItsTTL(t *testing.T) {
	ctx := context.Background()
	// One second, so the test does not sit out a real checkout window. EX takes
	// whole seconds, so this is the shortest honest TTL there is.
	const ttl = time.Second
	f := newFixture(t, ttl)
	eventID, seatID := seedOneSeat(t, f)

	held, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, uuid.Must(uuid.NewV7()).String(), "abandoned")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}

	// While the window is open, nobody else gets the seat.
	if _, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, uuid.Must(uuid.NewV7()).String(), "too-early"); !errors.Is(err, domain.ErrSeatUnavailable) {
		t.Fatalf("want ErrSeatUnavailable while the hold stands, got %v", err)
	}
	if got := holder(t, ctx, f, eventID, seatID); got != held.ReservationID {
		t.Fatalf("the seat should be held by %s, got %q", held.ReservationID, got)
	}

	// Wait for the TTL and a margin, doing nothing. No sweep, no tick, no job.
	deadline := time.Now().Add(5 * time.Second)
	for holder(t, ctx, f, eventID, seatID) != "" {
		if time.Now().After(deadline) {
			t.Fatalf("the hold on seat %s outlived its %s TTL", seatID, ttl)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The freed seat is immediately purchasable by someone else (PRD §4.1 step 7).
	next, err := f.holder.HoldSeats(ctx, eventID, []string{seatID}, uuid.Must(uuid.NewV7()).String(), "next-buyer")
	if err != nil {
		t.Fatalf("re-hold an expired seat: %v", err)
	}
	if next.ReservationID == held.ReservationID {
		t.Fatal("the second buyer somehow got the first reservation")
	}

	// The abandoned reservation is left pending in Postgres with a deadline in
	// the past, and nothing comes along to rewrite it. That is deliberate:
	// expiry is a fact about the clock, derivable from expires_at by anyone who
	// asks, and the saga refuses to charge against it (FR-4.2). Rewriting the
	// row would need a job scanning for closed windows - which is the sweeper,
	// under a new name.
	var status string
	var expiresAt time.Time
	if err := testPool.QueryRow(ctx,
		`SELECT status, expires_at FROM reservations WHERE id = $1`, held.ReservationID).Scan(&status, &expiresAt); err != nil {
		t.Fatalf("read reservation: %v", err)
	}
	if status != string(domain.ReservationPending) {
		t.Fatalf("want the abandoned reservation still pending (expiry is derived, not swept), got %s", status)
	}
	if !expiresAt.Before(time.Now()) {
		t.Fatalf("want a deadline in the past, got %s", expiresAt)
	}
	if _, err := f.saga.Pay(ctx, held.ReservationID, uuid.Must(uuid.NewV7()).String(), "late"); err == nil {
		t.Fatal("an expired reservation must not be payable")
	}
}

// TestHoldsFailClosedWhenRedisIsGone is D12, and it is a test about what the
// service refuses to do.
//
// Redis is the lock. With it gone there is no way to hand a seat to exactly one
// caller, so the only safe answer is to hand it to nobody: reject the request,
// loudly and distinguishably, and never fall back to an unlocked path
// (AGENTS.md §2 rule 11). A version of this code that "degraded gracefully" by
// holding seats in Postgres again would pass every other test in this package
// and sell the same seat twice the first time two customers arrived together.
//
// The Redis it kills is its own container, not the package's - the other tests
// still need theirs.
func TestHoldsFailClosedWhenRedisIsGone(t *testing.T) {
	ctx := context.Background()

	redisContainer, addr, err := startRedis(ctx)
	if err != nil {
		t.Fatalf("start a redis of our own: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(redisContainer) })

	f := newReplicaAt(t, time.Minute, addr)
	ev, seats, err := f.seeder.Seed(ctx, seedSpecFor(t, 2))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	first, second := seats[0].ID, seats[1].ID

	// A hold that works, so the failure below is Redis going away rather than
	// never having been there.
	if _, err := f.holder.HoldSeats(ctx, ev.ID, []string{first}, uuid.Must(uuid.NewV7()).String(), "before"); err != nil {
		t.Fatalf("hold before the outage: %v", err)
	}

	stopTimeout := 10 * time.Second
	if err := redisContainer.Stop(ctx, &stopTimeout); err != nil {
		t.Fatalf("stop redis: %v", err)
	}

	// Readiness says so first: a replica that cannot claim a seat should be
	// taken out of rotation, not left answering 503 to every customer.
	if err := f.holds.Ping(ctx); !errors.Is(err, domain.ErrHoldsUnavailable) {
		t.Fatalf("want the hold store to report itself unavailable, got %v", err)
	}

	_, err = f.holder.HoldSeats(ctx, ev.ID, []string{second}, uuid.Must(uuid.NewV7()).String(), "during")
	if !errors.Is(err, domain.ErrHoldsUnavailable) {
		t.Fatalf("want ErrHoldsUnavailable, got %v", err)
	}
	// Distinguishable from a lost race, because the two mean opposite things to
	// a client: one says the seat is gone, the other says try again shortly.
	if errors.Is(err, domain.ErrSeatUnavailable) {
		t.Fatal("a Redis outage must not be reported as a seat conflict")
	}

	// Nothing was written on the way to failing. No reservation, no claim, no
	// seat quietly moved - the request stopped at the lock it could not take.
	var reservations int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM reservations WHERE event_id = $1`, ev.ID).Scan(&reservations); err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	if reservations != 1 {
		t.Fatalf("want only the pre-outage reservation, got %d", reservations)
	}
	var claims int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM reservation_seats WHERE seat_id = $1`, second).Scan(&claims); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if claims != 0 {
		t.Fatalf("want no claim on the seat nobody could lock, got %d", claims)
	}
	after, err := f.repo.ListSeatsByEvent(ctx, ev.ID)
	if err != nil {
		t.Fatalf("list seats: %v", err)
	}
	for _, s := range after {
		if s.Status != domain.SeatAvailable {
			t.Fatalf("seat %s is %s: nothing may be sold while the lock is unreachable", s.ID, s.Status)
		}
	}
}
