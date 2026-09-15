-- Back to P0/P2 seat state: Postgres holds again, swept by held_until.
--
-- Holds themselves are not restored. They live in Redis, they have a TTL of
-- minutes, and a rollback that invented seat locks from claim rows would be
-- inventing state rather than restoring it.

-- P0's index is "one live claim per seat" and unconfirmed claims collide under
-- it, so settle them before it comes back.
UPDATE reservation_seats SET released_at = now() WHERE confirmed_at IS NULL AND released_at IS NULL;

DROP INDEX IF EXISTS one_confirmed_claim_per_seat;
CREATE UNIQUE INDEX one_active_claim_per_seat
    ON reservation_seats (seat_id) WHERE released_at IS NULL;
ALTER TABLE reservation_seats DROP COLUMN IF EXISTS confirmed_at;

ALTER TABLE seats DROP CONSTRAINT IF EXISTS seats_status_check;
ALTER TABLE seats ADD CONSTRAINT seats_status_check CHECK (status IN ('available', 'held', 'booked'));
ALTER TABLE seats ADD COLUMN held_by_reservation uuid REFERENCES reservations (id) ON DELETE SET NULL;
ALTER TABLE seats ADD COLUMN held_until timestamptz;
ALTER TABLE seats ADD CONSTRAINT seats_hold_metadata_consistent CHECK (
    (status = 'held' AND held_by_reservation IS NOT NULL AND held_until IS NOT NULL)
    OR (status <> 'held' AND held_by_reservation IS NULL AND held_until IS NULL)
);

CREATE INDEX seats_held_expiry_idx ON seats (held_until) WHERE status = 'held';
CREATE INDEX reservations_pending_expiry_idx ON reservations (expires_at) WHERE status = 'pending';
