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
SELECT id, event_id, section, "row", number, status, held_by_reservation, held_until, price_cents
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
SELECT id, event_id, section, "row", number, status, held_by_reservation, held_until, price_cents
FROM seats
WHERE event_id = $1
ORDER BY section, "row", number;

-- name: CreateReservation :exec
INSERT INTO reservations (id, user_id, event_id, status, total_cents, expires_at, idempotency_key)
VALUES (@id, @user_id, @event_id, @status, @total_cents, @expires_at, @idempotency_key);

-- name: GetReservationByIdempotencyKey :one
--
-- Idempotent retries (FR-3.7): the same key returns the first reservation
-- rather than claiming a second set of seats.
SELECT id, user_id, event_id, status, total_cents, expires_at, idempotency_key, created_at, payment_pending_since
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
--
-- payment_pending_since IS NULL is the D8 guard added in P2, and it is not
-- optional. A reservation whose payment outcome is unknown may already have
-- been charged; releasing its seats here would sell a seat the customer has
-- paid for - the exact mistake D8 forbids, reached through the sweeper rather
-- than through the saga. Those rows belong to the reconciliation job alone.
WITH expired AS (
    UPDATE reservations
    SET status = 'expired'
    WHERE status = 'pending'
      AND expires_at <= now()
      AND payment_pending_since IS NULL
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
INSERT INTO seats (id, event_id, section, "row", number, price_cents)
VALUES ($1, $2, $3, $4, $5, $6);

-- ---------------------------------------------------------------------------
-- P2: the reservation saga.
--
-- Every statement below runs inside a transaction opened by usecase. The
-- ordering discipline is P0's, for P0's reason: the reservation row is locked
-- before the seats it owns, on every path, so two concurrent saga steps queue
-- instead of forming a waiting cycle.
-- ---------------------------------------------------------------------------

-- name: LockReservationForUpdate :one
--
-- Takes the reservation write lock and re-reads its state under it.
--
-- Re-reading is the point. Between the payment call and this transaction the
-- sweeper may have expired the reservation, or a concurrent retry may have
-- confirmed it. The status read here is what the decision is made on; the
-- status the caller remembered from before the network hop is stale by
-- definition.
SELECT id, user_id, event_id, status, total_cents, expires_at, idempotency_key, created_at, payment_pending_since
FROM reservations
WHERE id = $1
FOR UPDATE;

-- name: GetReservationByID :one
SELECT id, user_id, event_id, status, total_cents, expires_at, idempotency_key, created_at, payment_pending_since
FROM reservations
WHERE id = $1;

-- name: LockSeatsByReservation :many
--
-- Locks every seat this reservation claims, in seat-ID order (D5). Same
-- ordering rule as the hold path, so a confirm, a release and a hold all walk
-- contended seat rows in the same sequence and no cycle can form between them.
SELECT s.id, s.event_id, s.section, s."row", s.number, s.status, s.held_by_reservation, s.held_until, s.price_cents
FROM seats s
JOIN reservation_seats rs ON rs.seat_id = s.id
WHERE rs.reservation_id = $1
  AND rs.released_at IS NULL
ORDER BY s.id
FOR UPDATE OF s;

-- name: MarkSeatsBooked :execrows
--
-- held -> booked. The hold metadata is cleared because a booked seat is claimed
-- permanently rather than until a deadline, and the seats_hold_metadata_consistent
-- constraint requires exactly that. status = 'held' is a tripwire: the caller
-- compares the row count against the seats it locked, so a seat that moved
-- underneath the transaction surfaces as an error, not a partial booking.
UPDATE seats
SET status              = 'booked',
    held_by_reservation = NULL,
    held_until          = NULL
WHERE held_by_reservation = @reservation_id
  AND status = 'held';

-- name: ReleaseReservationSeats :execrows
--
-- held -> available, with the claim stamped released so the D13 partial unique
-- index frees up for the next holder. One statement, so a claim can never be
-- stamped released while its seat stays held.
WITH released AS (
    UPDATE reservation_seats rs
    SET released_at = now()
    WHERE rs.reservation_id = @reservation_id
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

-- name: ReleaseBookedSeats :execrows
--
-- booked -> available, for a refund. Kept separate from ReleaseReservationSeats
-- because the state it moves from is different, and one statement accepting
-- either would happily release a seat from a state nobody intended.
WITH released AS (
    UPDATE reservation_seats rs
    SET released_at = now()
    WHERE rs.reservation_id = @reservation_id
      AND rs.released_at IS NULL
    RETURNING rs.seat_id
)
UPDATE seats s
SET status              = 'available',
    held_by_reservation = NULL,
    held_until          = NULL
FROM released r
WHERE s.id = r.seat_id
  AND s.status = 'booked';

-- name: SetReservationStatus :execrows
--
-- @current_status is the guard. Transitions are one-way (FR-5.1), and naming the state
-- being left means a concurrent writer that already moved the row cannot be
-- silently overwritten: the row count comes back zero and the caller knows it
-- lost the race.
UPDATE reservations
SET status = @new_status,
    -- Reaching a terminal state settles any payment doubt by definition.
    payment_pending_since = NULL
WHERE id = @id
  AND status = @current_status;

-- name: MarkPaymentPending :execrows
--
-- Hands the reservation to the reconciliation job (D8). Until this is cleared,
-- neither the sweeper nor anything else may release these seats: a charge may
-- exist for them and nobody yet knows its outcome.
--
-- Idempotent by construction. The first unknown outcome is the one that
-- matters, so a repeat leaves the original timestamp alone rather than pushing
-- the reconciliation grace period further into the future.
UPDATE reservations
SET payment_pending_since = now()
WHERE id = @id
  AND status = 'pending'
  AND payment_pending_since IS NULL;

-- name: ListReservationsAwaitingReconciliation :many
--
-- The reconciliation scan (ARCHITECTURE.md §6.3): reservations whose payment
-- outcome has been unknown for longer than the grace period.
--
-- Oldest first, so the longest-stuck reservation is resolved first, and limited
-- so one sweep cannot stall on a backlog.
SELECT id, user_id, event_id, status, total_cents, expires_at, idempotency_key, created_at, payment_pending_since
FROM reservations
WHERE status = 'pending'
  AND payment_pending_since IS NOT NULL
  AND payment_pending_since <= @older_than
ORDER BY payment_pending_since
LIMIT @row_limit;

-- name: CreateBooking :exec
--
-- The reservation_id UNIQUE constraint is what makes confirmation idempotent at
-- the database level: a retry, or reconciliation racing the live saga, collides
-- here instead of issuing a second set of tickets.
INSERT INTO bookings (id, reservation_id, user_id, payment_id, status, confirmed_at)
VALUES (@id, @reservation_id, @user_id, @payment_id, @status, @confirmed_at);

-- name: GetBookingByReservation :one
SELECT id, reservation_id, user_id, payment_id, status, confirmed_at
FROM bookings
WHERE reservation_id = $1;

-- name: SetBookingStatus :execrows
UPDATE bookings
SET status = @new_status
WHERE id = @id
  AND status = @current_status;

-- name: CreateTickets :copyfrom
INSERT INTO tickets (id, booking_id, seat_id, qr_code)
VALUES ($1, $2, $3, $4);

-- name: ListTicketsByBooking :many
SELECT id, booking_id, seat_id, qr_code, issued_at
FROM tickets
WHERE booking_id = $1
ORDER BY seat_id;
