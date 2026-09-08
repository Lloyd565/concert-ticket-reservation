-- Every query here is hand-written SQL compiled by sqlc into type-safe Go.
-- There is no ORM by design (AGENTS.md §2 rule 4): the locking semantics below
-- are the correctness core of this service and must be readable as SQL.

-- name: LockSeatsForUpdate :many
--
-- THE locking query. This is where the double-booking race is closed.
--
-- FOR UPDATE takes a row-level write lock on every matched seat and holds it
-- until this transaction commits or rolls back. A concurrent transaction
-- running this same query against any of these rows blocks here rather than
-- reading a stale 'available' - which is precisely the check-then-act window
-- that would otherwise let two requests both believe a seat is free.
--
-- ORDER BY id makes the lock acquisition order deterministic (D5). Postgres
-- applies the row-locking step above the sort, so rows are locked in id order.
-- Every transaction in the system therefore walks contended rows in the same
-- sequence and a waiting cycle - a deadlock - cannot form.
SELECT id, event_id, section, "row", number, status, held_by_reservation, held_until
FROM seats
WHERE event_id = $1
  AND id = ANY (@seat_ids::uuid[])
ORDER BY id
FOR UPDATE;

-- name: MarkSeatsHeld :execrows
--
-- The 'act' half of the check-then-act pair. Safe only because the caller is
-- inside the transaction that already locked these rows above. The redundant
-- status = 'available' predicate is a tripwire: the caller compares the
-- affected row count against the number of seats requested, so if the lock
-- were ever missing the mismatch surfaces as an error instead of a silent
-- partial hold.
UPDATE seats
SET status              = 'held',
    held_by_reservation = @reservation_id,
    held_until          = @held_until
WHERE id = ANY (@seat_ids::uuid[])
  AND status = 'available';

-- name: ListSeatsByEvent :many
SELECT id, event_id, section, "row", number, status, held_by_reservation, held_until
FROM seats
WHERE event_id = $1
ORDER BY section, "row", number;

-- name: CreateReservation :exec
INSERT INTO reservations (id, user_id, event_id, status, expires_at, idempotency_key)
VALUES (@id, @user_id, @event_id, @status, @expires_at, @idempotency_key);

-- name: GetReservationByIdempotencyKey :one
--
-- Idempotent retries (FR-3.7): the same key returns the first reservation
-- rather than claiming a second set of seats.
SELECT id, user_id, event_id, status, expires_at, idempotency_key, created_at
FROM reservations
WHERE idempotency_key = $1;

-- name: GetReservationSeatIDs :many
SELECT seat_id
FROM reservation_seats
WHERE reservation_id = $1
ORDER BY seat_id;

-- name: AttachSeatsToReservation :exec
--
-- One round trip for N seats. The partial unique index one_active_claim_per_seat
-- rejects this insert if any seat already has a live claim (D13 backstop).
INSERT INTO reservation_seats (reservation_id, seat_id)
SELECT @reservation_id, unnest(@seat_ids::uuid[]);

-- name: ReleaseExpiredHolds :execrows
--
-- The P0 expiry mechanism (ARCHITECTURE.md §5.2). One statement, so the three
-- writes commit together: a reservation can never be marked expired while its
-- seats stay held, or vice versa.
--
-- Chained CTEs run in a single snapshot: expire the pending reservations whose
-- checkout window closed, stamp their claims released (which frees the D13
-- index for the next holder), then return exactly those seats to available.
WITH expired AS (
    UPDATE reservations
    SET status = 'expired'
    WHERE status = 'pending'
      AND expires_at <= now()
    RETURNING id
), released AS (
    UPDATE reservation_seats rs
    SET released_at = now()
    FROM expired e
    WHERE rs.reservation_id = e.id
      AND rs.released_at IS NULL
    RETURNING rs.seat_id
)
UPDATE seats s
SET status              = 'available',
    held_by_reservation = NULL,
    held_until          = NULL
FROM released r
WHERE s.id = r.seat_id
  AND s.status = 'held';

-- name: CreateEvent :exec
INSERT INTO events (id, name, starts_at)
VALUES (@id, @name, @starts_at);

-- name: CreateSeats :copyfrom
--
-- Bulk seat-map insert for seeding. pgx COPY, one round trip for the whole map.
INSERT INTO seats (id, event_id, section, "row", number)
VALUES ($1, $2, $3, $4, $5);
