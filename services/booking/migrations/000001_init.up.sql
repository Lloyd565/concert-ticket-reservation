-- P0 schema for the Booking service: the seat-state authority.
-- Scope is deliberately narrow (ARCHITECTURE-ESSENTIALS.md build order):
-- venues, pricing_tiers, bookings, tickets and outbox arrive with the phases
-- that actually use them. Nothing here is edited later - new migrations only
-- (AGENTS.md §5).

CREATE TABLE events (
    id         uuid PRIMARY KEY,
    name       text        NOT NULL,
    starts_at  timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
    -- venue_id, organizer_id, on_sale_at and status land with the organizer
    -- flow; there is no Auth service in P0 to own organizer identity.
);

CREATE TABLE reservations (
    id        uuid PRIMARY KEY,
    user_id   uuid        NOT NULL, -- no FK: users live in auth_db (D2, no shared schemas)
    event_id  uuid        NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    status    text        NOT NULL CHECK (status IN ('pending', 'confirmed', 'expired', 'failed', 'refunded')),
    expires_at timestamptz NOT NULL,
    -- Every mutating operation carries one (AGENTS.md §2 rule 10); UNIQUE is
    -- what makes a retry return the first result instead of holding twice.
    idempotency_key text   NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
    -- total_cents arrives with the Payment service in P2; P0 has no pricing.
);

-- Drives the sweeper's scan for expired checkout windows.
CREATE INDEX reservations_pending_expiry_idx ON reservations (expires_at) WHERE status = 'pending';

CREATE TABLE seats (
    id       uuid PRIMARY KEY,
    event_id uuid NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    section  text NOT NULL,
    "row"    text NOT NULL, -- quoted: ROW is a keyword in Postgres
    number   text NOT NULL,
    status   text NOT NULL DEFAULT 'available' CHECK (status IN ('available', 'held', 'booked')),
    held_by_reservation uuid REFERENCES reservations (id) ON DELETE SET NULL,
    held_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),

    UNIQUE (event_id, section, "row", number),

    -- A held seat carries both a holder and a deadline; a seat in any other
    -- state carries neither. Without this a bug could leave held_until set on
    -- an available seat and the sweeper would "release" a free seat forever.
    CONSTRAINT seats_hold_metadata_consistent CHECK (
        (status = 'held' AND held_by_reservation IS NOT NULL AND held_until IS NOT NULL)
        OR (status <> 'held' AND held_by_reservation IS NULL AND held_until IS NULL)
    )
);

-- Seat-map reads filter by event and status (FR-2.4).
CREATE INDEX seats_event_status_idx ON seats (event_id, status);
-- Sweeper scan: only held seats can expire, so the index only covers them.
CREATE INDEX seats_held_expiry_idx ON seats (held_until) WHERE status = 'held';

CREATE TABLE reservation_seats (
    reservation_id uuid NOT NULL REFERENCES reservations (id) ON DELETE CASCADE,
    seat_id        uuid NOT NULL REFERENCES seats (id) ON DELETE CASCADE,
    -- NULL while this claim is live; stamped when the claim is released,
    -- expired or refunded. This column is what makes the backstop below
    -- expressible as a partial index.
    released_at    timestamptz,
    PRIMARY KEY (reservation_id, seat_id)
);

-- D13, the database-level backstop. The application already guarantees one
-- active claim per seat via SELECT ... FOR UPDATE; this index means that if the
-- application logic is ever wrong, Postgres still refuses the double-booking
-- rather than recording it. Defense in depth: two independent mechanisms must
-- both fail before a seat is sold twice.
CREATE UNIQUE INDEX one_active_claim_per_seat
    ON reservation_seats (seat_id) WHERE released_at IS NULL;
