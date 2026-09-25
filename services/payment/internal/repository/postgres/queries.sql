-- Hand-written SQL compiled by sqlc into type-safe Go. No ORM by design
-- (AGENTS.md §2 rule 4).
--
-- The important property in this file is not locking - Payment has no contended
-- row - but ordering: the INSERT below happens before any provider call, and
-- the settle statements below only ever move a row out of 'pending'.

-- name: CreateCharge :exec
--
-- Written BEFORE the provider is called (FR-4.5). The UNIQUE index on
-- idempotency_key is what turns a concurrent double-submit into one charge:
-- both callers pass an application-level "have I seen this key" check, and only
-- one of them survives this INSERT.
INSERT INTO charges (id, reservation_id, user_id, amount_cents, status, idempotency_key, created_at, updated_at)
VALUES (@id, @reservation_id, @user_id, @amount_cents, @status, @idempotency_key, @created_at, @updated_at);

-- name: GetChargeByIdempotencyKey :one
--
-- The replay lookup (FR-4.3), and the query Booking's reconciliation job uses
-- to resolve an unknown payment outcome (ARCHITECTURE.md §6.3).
SELECT id, reservation_id, user_id, amount_cents, status, provider_ref, decline_reason, idempotency_key, created_at, updated_at
FROM charges
WHERE idempotency_key = $1;

-- name: GetChargeByID :one
SELECT id, reservation_id, user_id, amount_cents, status, provider_ref, decline_reason, idempotency_key, created_at, updated_at
FROM charges
WHERE id = $1;

-- name: SettleCharge :one
--
-- status = 'pending' in the WHERE clause is the guard, not decoration: two
-- attempts under one key can both get an answer from the provider, and the
-- first settled result is the authoritative one. Without this predicate the
-- later writer would silently overwrite it - which for a succeeded charge means
-- losing the provider reference the refund depends on.
--
-- P4: the settlement's event is written by the same statement (D9). The outbox
-- row is selected FROM the update, so it exists exactly when the update moved a
-- row: a losing duplicate settles nothing and announces nothing, and there is no
-- moment at which the charge is settled but its event is not yet recorded.
-- Returns how many charges were settled: 1, or 0 for the loser.
WITH settled AS (
    UPDATE charges
    SET status         = @status,
        provider_ref   = @provider_ref,
        decline_reason = @decline_reason,
        updated_at     = @updated_at
    WHERE charges.id = @id
      AND charges.status = 'pending'
    RETURNING charges.id
), announced AS (
    INSERT INTO outbox (id, aggregate_id, event_type, payload, created_at)
    SELECT @event_id::uuid, settled.id, @event_type::text, @payload::jsonb, @updated_at
    FROM settled
)
SELECT count(*) FROM settled;

-- name: CreateRefund :exec
INSERT INTO refunds (id, charge_id, amount_cents, status, idempotency_key, created_at, updated_at)
VALUES (@id, @charge_id, @amount_cents, @status, @idempotency_key, @created_at, @updated_at);

-- name: GetRefundByIdempotencyKey :one
SELECT id, charge_id, amount_cents, status, provider_ref, idempotency_key, created_at, updated_at
FROM refunds
WHERE idempotency_key = $1;

-- name: SettleRefund :one
--
-- Same guard as SettleCharge, for the same reason, and the same single
-- statement for the event. A refund that failed announces nothing: @event_type
-- is empty and the INSERT selects no row.
WITH settled AS (
    UPDATE refunds
    SET status       = @status,
        provider_ref = @provider_ref,
        updated_at   = @updated_at
    WHERE refunds.id = @id
      AND refunds.status = 'pending'
    RETURNING refunds.id
), announced AS (
    INSERT INTO outbox (id, aggregate_id, event_type, payload, created_at)
    SELECT @event_id::uuid, settled.id, @event_type::text, @payload::jsonb, @updated_at
    FROM settled
    WHERE @event_type::text <> ''
)
SELECT count(*) FROM settled;

-- name: ClaimOutboxBatch :many
--
-- Leases a batch of unpublished rows to one relay, and commits at once. No lock
-- is held while publishing (D4); the lease is what keeps two relays apart, and a
-- lapsed one makes the row claimable again. Measured on the database clock, the
-- clock that checks it. RETURNING has no order; the caller sorts.
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
-- Runs only for rows the broker has confirmed. A crash before this line means a
-- duplicate publish once the lease lapses - never a loss.
UPDATE outbox
SET published_at = now()
WHERE id = ANY (@ids::uuid[]);
