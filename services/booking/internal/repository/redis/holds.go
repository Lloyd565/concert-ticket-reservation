// Package redis implements the seat-hold port declared in usecase. It is the
// only place in the service that imports a Redis client (AGENTS.md §4).
//
// This is where the double-booking race is closed in P3. Postgres still owns
// every confirmed booking, and confirmation still takes row locks - but the
// contended step, the one thousands of callers hit at once for the same seat,
// is a single Redis command that either sets a key or does not.
package redis

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
)

// Key layout, fixed by ARCHITECTURE.md §5.2:
//
//	seat:hold:{event_id}:{seat_id} -> {reservation_id}, with a TTL
//
// The value is the holder's identity, and every write below checks it before
// acting. A blind DEL would let a caller whose hold expired ten seconds ago
// delete the hold of whoever legitimately took the seat next.
const keyPrefix = "seat:hold:"

// claimScript is the all-or-nothing multi-seat claim (D6).
//
// Redis runs a script to completion with nothing else interleaved, so every
// SET NX in this loop, and the rollback that undoes them, happen as one
// indivisible step from the point of view of every other client. That is the
// whole reason it is a script rather than a sequence of commands from Go - see
// the comment on Claim.
//
// Returns 0 on success, or the 1-based index of the first seat that was already
// taken, so the caller can name the seat it lost.
var claimScript = redis.NewScript(`
for i = 1, #KEYS do
  if not redis.call('SET', KEYS[i], ARGV[1], 'NX', 'EX', ARGV[2]) then
    -- Undo the claims already taken in this pass. A partial hold is a defect
    -- (D6), and leaving one here would strand seats for a whole TTL under a
    -- reservation that never existed.
    for j = 1, i - 1 do
      redis.call('DEL', KEYS[j])
    end
    return i
  end
end
return 0
`)

// releaseScript drops only the keys this reservation still owns.
//
// The ownership check is not politeness. Between a hold expiring and this
// running, another caller may hold the seat; deleting their key would hand the
// same seat to a third caller while the second is at the checkout page.
var releaseScript = redis.NewScript(`
local n = 0
for i = 1, #KEYS do
  if redis.call('GET', KEYS[i]) == ARGV[1] then
    n = n + redis.call('DEL', KEYS[i])
  end
end
return n
`)

// extendScript pushes out the expiry of the keys this reservation still owns.
//
// This is the P3 half of D8. When a payment outcome is unknown the seats must
// not go back on sale - and a TTL does not know that, so somebody has to tell
// it. Same ownership check, same reason.
var extendScript = redis.NewScript(`
local n = 0
for i = 1, #KEYS do
  if redis.call('GET', KEYS[i]) == ARGV[1] then
    n = n + redis.call('EXPIRE', KEYS[i], ARGV[2])
  end
end
return n
`)

// Holds implements usecase.HoldStore against Redis.
type Holds struct{ client redis.UniversalClient }

// NewHolds returns a Holds backed by client.
func NewHolds(client redis.UniversalClient) *Holds { return &Holds{client: client} }

// Claim atomically claims every seat for reservationID, or claims none.
//
// The seats are claimed in sorted seat-ID order (D5), consistently with every
// other multi-seat path in the system.
func (h *Holds) Claim(ctx context.Context, eventID string, seatIDs []string, reservationID string, ttl time.Duration) error {
	keys := h.keys(eventID, seatIDs)
	taken, err := claimScript.Run(ctx, h.client, keys, reservationID, seconds(ttl)).Int()
	if err != nil {
		return unavailable("claim seats", err)
	}
	if taken > 0 {
		// taken is 1-based into keys, which is sorted the same way seatIDs is.
		return fmt.Errorf("seat %s is already held: %w", sorted(seatIDs)[taken-1], domain.ErrSeatUnavailable)
	}
	return nil
}

// Release drops the claims this reservation still owns and reports how many
// were dropped.
func (h *Holds) Release(ctx context.Context, eventID string, seatIDs []string, reservationID string) (int, error) {
	n, err := releaseScript.Run(ctx, h.client, h.keys(eventID, seatIDs), reservationID).Int()
	if err != nil {
		return 0, unavailable("release seats", err)
	}
	return n, nil
}

// Extend pushes the expiry of the claims this reservation still owns out to
// ttl from now, and reports how many were extended.
func (h *Holds) Extend(ctx context.Context, eventID string, seatIDs []string, reservationID string, ttl time.Duration) (int, error) {
	n, err := extendScript.Run(ctx, h.client, h.keys(eventID, seatIDs), reservationID, seconds(ttl)).Int()
	if err != nil {
		return 0, unavailable("extend hold", err)
	}
	return n, nil
}

// Owns reports whether reservationID still holds every one of these seats.
//
// One MGET rather than a script: this is a read, and the answer is advisory by
// the time it returns anyway. What makes confirmation safe is the row lock the
// caller takes next, not this - see the comment in Saga.ConfirmPaid.
func (h *Holds) Owns(ctx context.Context, eventID string, seatIDs []string, reservationID string) (bool, error) {
	if len(seatIDs) == 0 {
		// A reservation with no seats holds nothing. Answering false beats
		// sending MGET no arguments and reporting Redis's protocol error as an
		// outage.
		return false, nil
	}
	values, err := h.client.MGet(ctx, h.keys(eventID, seatIDs)...).Result()
	if err != nil {
		return false, unavailable("read holds", err)
	}
	for _, v := range values {
		if v != reservationID {
			return false, nil
		}
	}
	return true, nil
}

// Ping reports whether holds can be taken at all. Readiness depends on it: a
// Booking replica that cannot reach Redis cannot claim a seat, so it is not
// ready (D12, NFR-4.4).
func (h *Holds) Ping(ctx context.Context) error {
	if err := h.client.Ping(ctx).Err(); err != nil {
		return unavailable("ping", err)
	}
	return nil
}

// keys builds the hold keys in sorted seat-ID order (D5).
//
// Ordering matters even inside a script that cannot be interleaved: the script
// is atomic per Redis node, and pinning the order keeps the semantics identical
// if this ever runs against a cluster, where the keys of one multi-seat request
// may live on different nodes.
func (h *Holds) keys(eventID string, seatIDs []string) []string {
	ids := sorted(seatIDs)
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, keyPrefix+eventID+":"+id)
	}
	return keys
}

func sorted(seatIDs []string) []string {
	ids := append([]string(nil), seatIDs...)
	sort.Strings(ids)
	return ids
}

// seconds rounds a TTL up to whole seconds, which is what EX takes. Rounding up
// rather than down: a hold that is a fraction of a second short of its
// advertised window would expire while the customer still sees a countdown.
func seconds(ttl time.Duration) string {
	s := int64(ttl / time.Second)
	if ttl%time.Second != 0 {
		s++
	}
	if s < 1 {
		s = 1
	}
	return strconv.FormatInt(s, 10)
}

// unavailable turns any Redis failure into the one error D12 is about.
//
// Every caller of this package must fail closed, and the way to make that hard
// to get wrong is to give them a single error that says so. A timeout, a
// refused connection and a script error are all the same thing here: the lock
// is not available, so the seats must not be handed out.
func unavailable(op string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// The caller gave up, not Redis. Still fail closed - nothing was
		// claimed - but do not report it as an outage.
		return fmt.Errorf("%s: %w", op, err)
	}
	return fmt.Errorf("%s (%v): %w", op, err, domain.ErrHoldsUnavailable)
}
