-- P3: Redis owns holds; Postgres owns confirmed bookings.
--
-- Nothing in 000001 or 000002 is edited (AGENTS.md §5) - this migration only
-- adds and drops.
--
-- The seat row stops carrying a hold. A hold is transient state with a TTL, and
-- Redis expires it without anybody sweeping; a timestamp column cannot expire
-- itself, which is why P0 needed a sweeper at all. What stays here is the thing
-- that must survive a Redis restart: the confirmed booking.

-- Any hold still recorded in Postgres is at most one checkout window of state,
-- and Redis starts empty, so there is nothing to migrate - only to release.
-- Doing it in this order (claims, then reservations, then seats) leaves no
-- moment where a seat is available while a live claim still points at it.
UPDATE reservation_seats rs
SET released_at = now()
FROM seats s
WHERE s.id = rs.seat_id
  AND s.status = 'held'
  AND rs.released_at IS NULL;

UPDATE reservations SET status = 'expired' WHERE status = 'pending';

UPDATE seats
SET status              = 'available',
    held_by_reservation = NULL,
    held_until          = NULL
WHERE status = 'held';

-- The sweeper's two scans. Both are retired with it: Redis TTL is the expiry
-- mechanism now, and nothing scans for a closed checkout window any more.
DROP INDEX IF EXISTS seats_held_expiry_idx;
DROP INDEX IF EXISTS reservations_pending_expiry_idx;

-- held_by_reservation and held_until described a hold that lived here. It does
-- not any more, and leaving the columns would invite a code path to write a
-- hold that nothing expires.
ALTER TABLE seats DROP CONSTRAINT seats_hold_metadata_consistent;
ALTER TABLE seats DROP COLUMN held_by_reservation;
ALTER TABLE seats DROP COLUMN held_until;

-- A seat row is now available or booked. 'held' is not a state Postgres can
-- represent, which is the point: there is exactly one authority for a hold and
-- it is Redis.
ALTER TABLE seats DROP CONSTRAINT seats_status_check;
ALTER TABLE seats ADD CONSTRAINT seats_status_check CHECK (status IN ('available', 'booked'));

-- The claim rows stay - they are how a reservation remembers which seats it is
-- for, which confirmation needs and Redis cannot answer (its keys map seat to
-- reservation, not the reverse). What changes is when a claim becomes binding.
ALTER TABLE reservation_seats ADD COLUMN confirmed_at timestamptz;

-- D13, restated for P3.
--
-- In P0 the backstop was "one live claim per seat", because a claim was created
-- the moment a seat was held and Postgres was the hold authority. It cannot mean
-- that now: an abandoned hold expires in Redis with no writer left to stamp its
-- Postgres claim released, so a stale unconfirmed claim would block the seat
-- forever - the seat leak that retiring the sweeper is supposed to remove.
--
-- So the backstop now enforces the invariant itself, word for word: a given seat
-- is CONFIRMED to at most one booking. Unconfirmed claims are inert; the index
-- ignores them. If the Redis claim path is ever wrong, this still refuses the
-- double-booking.
DROP INDEX one_active_claim_per_seat;
CREATE UNIQUE INDEX one_confirmed_claim_per_seat
    ON reservation_seats (seat_id) WHERE confirmed_at IS NOT NULL AND released_at IS NULL;
