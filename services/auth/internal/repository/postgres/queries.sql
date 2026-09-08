-- Hand-written SQL compiled by sqlc into type-safe Go. No ORM (AGENTS.md §2
-- rule 4). Nothing here contends for a hot row, so nothing here locks: unlike
-- Booking, Auth has no shared mutable resource to race for.

-- name: CreateUser :exec
INSERT INTO users (id, email, password_hash, role, created_at)
VALUES ($1, $2, $3, $4, $5);

-- name: GetUserByEmail :one
SELECT id, email, password_hash, role, created_at
FROM users
WHERE email = $1;

-- name: GetUserByID :one
SELECT id, email, password_hash, role, created_at
FROM users
WHERE id = $1;

-- name: CreateRefreshToken :exec
INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5);

-- name: GetRefreshToken :one
SELECT id, user_id, token_hash, expires_at, revoked_at
FROM refresh_tokens
WHERE token_hash = $1;

-- name: RevokeRefreshToken :one
--
-- The WHERE clause is what makes logout idempotent and rotation safe: only a
-- live token is revoked, so a replayed logout matches no row and reports
-- revoked=false rather than re-stamping a revocation time.
UPDATE refresh_tokens
SET revoked_at = now()
WHERE token_hash = $1 AND revoked_at IS NULL
RETURNING id;

-- name: RevokeUserRefreshTokens :exec
-- Used when a rotated token is replayed: the replay is evidence the token
-- leaked, so every live session for that user is cut.
UPDATE refresh_tokens
SET revoked_at = now()
WHERE user_id = $1 AND revoked_at IS NULL;

-- name: DeleteExpiredRefreshTokens :exec
DELETE FROM refresh_tokens WHERE expires_at < now();

-- name: AdminExists :one
--
-- The guard for the startup admin bootstrap. EXISTS rather than COUNT: the
-- question is "is there at least one", and EXISTS stops at the first row.
SELECT EXISTS (SELECT 1 FROM users WHERE role = 'admin');
