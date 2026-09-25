-- P2: what the reservation saga needs on top of P0's seat state.
--
-- Nothing in 000001 is edited (AGENTS.md §5) - this migration only adds.

-- Price. P2 charges a flat per-seat rate rather than resolving a pricing tier:
-- pricing_tiers and seats.tier_id belong to the organizer flow, which is not
-- this phase, and the saga needs an amount, not a price list.
-- ponytail: flat price per seat. Becomes seats.tier_id -> pricing_tiers when
-- the organizer flow lands; total_cents below is already the sum, so the saga
-- and Payment do not change when it does.
ALTER TABLE seats ADD COLUMN price_cents bigint NOT NULL DEFAULT 0 CHECK (price_cents >= 0);

-- What the reservation is worth, fixed at hold time. Stored rather than
-- recomputed at payment time: re-summing later would let a price change between
-- the hold and the charge move the amount out from under the customer.
ALTER TABLE reservations ADD COLUMN total_cents bigint NOT NULL DEFAULT 0 CHECK (total_cents >= 0);

-- The D8 marker, and the most load-bearing column in this migration.
--
-- Set when a payment call returns an UNKNOWN outcome. It means: a charge may
-- exist for this reservation, so these seats must not be released by anyone
-- except the reconciliation job, which resolves them against Payment by
-- idempotency key.
--
-- Without it the P0 sweeper would blind-release these seats ten minutes later -
-- violating D8 through a different code path than the obvious one, and selling
-- a seat the customer may already have paid for. ReleaseExpiredHolds therefore
-- skips every row where this is set.
ALTER TABLE reservations ADD COLUMN payment_pending_since timestamptz;

-- The reconciliation job's scan (ARCHITECTURE.md §6.3).
CREATE INDEX reservations_awaiting_reconciliation_idx
    ON reservations (payment_pending_since)
    WHERE status = 'pending' AND payment_pending_since IS NOT NULL;

CREATE TABLE bookings (
    id             uuid PRIMARY KEY,
    -- One booking per reservation, enforced rather than assumed: a duplicate
    -- confirm - from a retry, or from reconciliation racing the live saga -
    -- must collide here rather than issue a second set of tickets.
    reservation_id uuid NOT NULL UNIQUE REFERENCES reservations (id) ON DELETE CASCADE,
    user_id        uuid NOT NULL, -- no FK: users live in auth_db (D2)
    -- The charge this booking was paid by. No FK: charges live in payment_db.
    payment_id     uuid NOT NULL,
    status         text NOT NULL CHECK (status IN ('confirmed', 'refunded')),
    confirmed_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX bookings_user_idx ON bookings (user_id);

CREATE TABLE tickets (
    id         uuid PRIMARY KEY,
    booking_id uuid NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    seat_id    uuid NOT NULL REFERENCES seats (id),
    -- The thing that gets scanned at the door. UNIQUE because two valid tickets
    -- with the same code is the physical-world version of a double-booking.
    qr_code    text NOT NULL UNIQUE,
    issued_at  timestamptz NOT NULL DEFAULT now(),

    -- One ticket per seat per booking.
    UNIQUE (booking_id, seat_id)
);

CREATE INDEX tickets_booking_idx ON tickets (booking_id);
