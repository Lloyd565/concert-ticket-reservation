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

-- name: SettleCharge :execrows
--
-- status = 'pending' in the WHERE clause is the guard, not decoration: two
-- attempts under one key can both get an answer from the provider, and the
-- first settled result is the authoritative one. Without this predicate the
-- later writer would silently overwrite it - which for a succeeded charge means
-- losing the provider reference the refund depends on.
UPDATE charges
SET status         = @status,
    provider_ref   = @provider_ref,
    decline_reason = @decline_reason,
    updated_at     = @updated_at
WHERE id = @id
  AND status = 'pending';

-- name: CreateRefund :exec
INSERT INTO refunds (id, charge_id, amount_cents, status, idempotency_key, created_at, updated_at)
VALUES (@id, @charge_id, @amount_cents, @status, @idempotency_key, @created_at, @updated_at);

-- name: GetRefundByIdempotencyKey :one
SELECT id, charge_id, amount_cents, status, provider_ref, idempotency_key, created_at, updated_at
FROM refunds
WHERE idempotency_key = $1;

-- name: SettleRefund :execrows
--
-- Same guard as SettleCharge, for the same reason.
UPDATE refunds
SET status       = @status,
    provider_ref = @provider_ref,
    updated_at   = @updated_at
WHERE id = @id
  AND status = 'pending';
