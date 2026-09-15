-- Every query here is hand-written SQL compiled by sqlc into type-safe Go.
-- There is no ORM by design (AGENTS.md §2 rule 4): the locking semantics below
-- are the correctness core of this service and must be readable as SQL.
--
-- P3 moved the hold out of this file. A hold is a Redis key with a TTL now
-- (repository/redis), so nothing here claims a seat; what is left is the
-- catalog, the reservation record, and the confirmation - the states that have
-- to outlive a Redis restart.

-- name: GetSeatsForEvent :many
--
-- Reads the requested seats' catalog rows: their prices, and whether they have
-- already been sold.
--
-- No FOR UPDATE, and that is not an oversight. In P0 this query WAS the lock -
-- the row lock it took is what closed the check-then-act race. In P3 the lock is
-- the Redis SET NX the caller has already won before reaching here, so this is
-- a read: do these seats exist, what do they cost, and is one of them booked?
--
-- The caller runs it AFTER taking the Redis claim, never before. Confirmation
-- commits the booked row and only then deletes the Redis key, so a claim won
-- after that delete is guaranteed to see 'booked' here. Reading first would
-- reopen exactly the window that ordering closes.
SELECT id, event_id, section, "row", number, status, price_cents
FROM seats
WHERE event_id = $1
  AND id = ANY (@seat_ids::uuid[])
ORDER BY id;

-- name: ListSeatsByEvent :many
--
-- The catalog view: available or booked. A seat held by somebody's open
-- checkout still reads 'available' here, because that fact lives in Redis.
SELECT id, event_id, section, "row", number, status, price_cents
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
-- Records which seats this reservation is for. One round trip for N seats.
--
-- These rows are not the claim on the seat - the Redis key is - so this cannot
-- trip the D13 index and does not try to. It exists because confirmation has to
-- know which seats to book, and Redis cannot answer that: its keys map seat to
-- reservation, never the reverse.
INSERT INTO reservation_seats (reservation_id, seat_id)
SELECT @reservation_id, unnest(@seat_ids::uuid[]);

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
-- Redis hold may have expired and the seats gone to somebody else, or a
-- concurrent retry may have confirmed it. The status read here is what the
-- decision is made on; the status the caller remembered from before the network
-- hop is stale by definition.
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
-- THE locking query in P3. Confirmation is where a transient Redis hold becomes
-- a permanent Postgres fact, so it is where Postgres has to be certain, and FOR
-- UPDATE is what makes it so: two reservations that both believe they hold this
-- seat - because a hold expired and was re-taken while a charge was in flight -
-- queue here instead of both reading 'available'.
--
-- ORDER BY s.id (D5), so the confirm and refund paths walk contended rows in the
-- same sequence and no waiting cycle can form between them.
SELECT s.id, s.event_id, s.section, s."row", s.number, s.status, s.price_cents
FROM seats s
JOIN reservation_seats rs ON rs.seat_id = s.id
WHERE rs.reservation_id = $1
  AND rs.released_at IS NULL
ORDER BY s.id
FOR UPDATE OF s;

-- name: MarkSeatsBooked :execrows
--
-- available -> booked, with the claim stamped confirmed at the same instant, in
-- one statement so the seat row and the D13 index can never disagree about who
-- owns the seat.
--
-- status = 'available' is the tripwire the caller counts against the seats it
-- locked above: if another reservation booked this seat first, the count comes
-- back short and confirmation fails rather than silently overwriting a sale.
WITH confirmed AS (
    UPDATE reservation_seats rs
    SET confirmed_at = now()
    WHERE rs.reservation_id = @reservation_id
      AND rs.released_at IS NULL
    RETURNING rs.seat_id
)
UPDATE seats s
SET status = 'booked'
FROM confirmed c
WHERE s.id = c.seat_id
  AND s.status = 'available';

-- name: ReleaseReservationClaims :execrows
--
-- Settles an unconfirmed reservation's claim rows without touching a seat -
-- because in P3 an unconfirmed claim never owned the seat in the first place;
-- the Redis key did, and the caller deletes that separately.
--
-- Bookkeeping, not compensation. It is what makes "this reservation is over"
-- legible in the database rather than only in Redis's absence.
UPDATE reservation_seats
SET released_at = now()
WHERE reservation_id = @reservation_id
  AND released_at IS NULL
  AND confirmed_at IS NULL;

-- name: ReleaseBookedSeats :execrows
--
-- booked -> available, for a refund, with the claim stamped released so the D13
-- index frees up for the next buyer. One statement, so a claim can never be
-- stamped released while its seat stays booked.
WITH released AS (
    UPDATE reservation_seats rs
    SET released_at = now()
    WHERE rs.reservation_id = @reservation_id
      AND rs.released_at IS NULL
    RETURNING rs.seat_id
)
UPDATE seats s
SET status = 'available'
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
-- nothing else may release these seats: a charge may exist for them and nobody
-- yet knows its outcome.
--
-- In P0 this column also told the sweeper to keep its hands off. There is no
-- sweeper now, so this is only half of D8 - the caller must also push the Redis
-- TTL out, or the hold simply expires and the seats go back on sale under a
-- charge that may have succeeded. See Saga.LeavePendingForReconciliation.
--
-- It is also the fence in front of every charge. The row count says whether the
-- reservation was still pending at this instant, and the saga calls Payment only
-- if it was. Once a release commits no new charge can start for it, so any
-- charge a release missed was already under way - which is what bounds how long
-- reconciliation waits before rechecking (migration 000004).
--
-- Idempotent by construction. The first mark is the one that matters, so a
-- repeat keeps the original timestamp rather than pushing the reconciliation
-- grace period further into the future - and still counts the row, because the
-- fence is about the status, not about who marked first.
UPDATE reservations
SET payment_pending_since = COALESCE(payment_pending_since, now())
WHERE id = @id
  AND status = 'pending';

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

-- name: QueueChargeRecheck :exec
--
-- Queues a reservation just released on "no charge" evidence for one more look
-- (migration 000004). Runs in the release's transaction: a release that commits
-- without it is exactly the orphan the table exists to prevent.
INSERT INTO charge_rechecks (reservation_id)
VALUES (@reservation_id)
ON CONFLICT (reservation_id) DO NOTHING;

-- name: ListReservationsAwaitingChargeRecheck :many
--
-- Released reservations whose release is older than the grace period: long
-- enough ago that a charge started before the release has reached Payment, if
-- it ever will. Oldest first, and limited, like the reconciliation scan.
SELECT r.id, r.user_id, r.event_id, r.status, r.total_cents, r.expires_at, r.idempotency_key, r.created_at, r.payment_pending_since
FROM charge_rechecks c
JOIN reservations r ON r.id = c.reservation_id
WHERE c.released_at <= @older_than
ORDER BY c.released_at
LIMIT @row_limit;

-- name: ClearChargeRecheck :exec
DELETE FROM charge_rechecks
WHERE reservation_id = @reservation_id;

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

-- ---------------------------------------------------------------------------
-- P4: the transactional outbox (D9, migration 000005).
-- ---------------------------------------------------------------------------

-- name: InsertOutboxEvent :exec
--
-- Only ever run inside the transaction of the state change the event describes
-- (Repo.Enqueue refuses otherwise). That shared transaction is the whole
-- pattern: the event commits with the change or not at all.
INSERT INTO outbox (id, aggregate_id, event_type, payload, created_at)
VALUES (@id, @aggregate_id, @event_type, @payload, @created_at);

-- name: ClaimOutboxBatch :many
--
-- Leases a batch of unpublished rows to one relay, and commits at once.
--
-- No lock is held while publishing: the relay calls RabbitMQ after this
-- statement returns, and a transaction may not span that call (D4). SKIP LOCKED
-- only keeps two replicas' claim statements from queueing behind each other; it
-- is the lease that keeps them from publishing the same row. A lapsed lease -
-- the relay died, or the broker never confirmed - makes the row claimable again.
--
-- The lease is measured on the database clock, the same clock that checks it, so
-- skew between replicas cannot shorten one. It is built with make_interval
-- because sqlc mis-rewrites a named parameter multiplied by an interval literal.
-- RETURNING has no order; the caller sorts by created_at.
WITH batch AS (
    SELECT o.id
    FROM outbox o
    WHERE o.published_at IS NULL
      AND (o.claimed_until IS NULL OR o.claimed_until < now())
    ORDER BY o.created_at
    LIMIT @batch_size
    FOR UPDATE SKIP LOCKED
)
UPDATE outbox
SET claimed_until = now() + make_interval(0, 0, 0, 0, 0, 0, @lease_seconds::float8)
FROM batch
WHERE outbox.id = batch.id
RETURNING outbox.id, outbox.event_type, outbox.payload, outbox.created_at;

-- name: MarkOutboxPublished :exec
--
-- Runs only for rows the broker has confirmed. A crash before this line means
-- the row is published again once its lease lapses: a duplicate, which every
-- consumer is required to survive (D10) - never a loss.
UPDATE outbox
SET published_at = now()
WHERE id = ANY (@ids::uuid[]);

-- name: ExpireLapsedReservations :many
--
-- Settles reservations whose checkout window closed unpaid, so reservation.expired
-- can be written in the transaction that settles them.
--
-- This is not the sweeper coming back. It releases no seat and deletes no Redis
-- key - the TTL still does that, unaided (§6.2 row 3). It moves a status and
-- nothing else.
--
-- payment_pending_since IS NULL is D8. A reservation with a charge in doubt
-- belongs to reconciliation, and expiring it here would turn a possibly-paid
-- customer's reservation into one confirmation refuses. The predicate is checked
-- again on the locked row, so a mark written by Pay between the scan and the
-- update wins; and Pay's own mark only counts a row still 'pending', so an
-- expiry that commits first stops the charge from starting.
WITH lapsed AS (
    SELECT r.id
    FROM reservations r
    WHERE r.status = 'pending'
      AND r.payment_pending_since IS NULL
      AND r.expires_at <= now()
    ORDER BY r.expires_at
    LIMIT @row_limit
    FOR UPDATE SKIP LOCKED
)
UPDATE reservations
SET status = 'expired'
FROM lapsed
WHERE reservations.id = lapsed.id
  AND reservations.status = 'pending'
  AND reservations.payment_pending_since IS NULL
RETURNING reservations.id, reservations.user_id, reservations.event_id, reservations.expires_at;
