-- P1 schema for the Auth service (ARCHITECTURE.md §8, auth_db).
--
-- This database is reachable only by this service (D2). booking_db stores
-- reservations.user_id with no foreign key precisely because these tables live
-- behind a service boundary, not behind a JOIN.

CREATE TABLE users (
    id            uuid PRIMARY KEY,
    -- Citext would be nicer but needs an extension; the application lowercases
    -- on the way in and this index is what makes that normalisation binding.
    email         text NOT NULL UNIQUE,
    -- The full PHC-format Argon2id string: algorithm, parameters, salt and
    -- digest together. Storing the parameters alongside the digest is what
    -- lets the cost be raised later without invalidating existing passwords.
    password_hash text NOT NULL,
    role          text NOT NULL CHECK (role IN ('attendee', 'organizer', 'admin')),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE refresh_tokens (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- SHA-256 of the token, never the token itself: a database disclosure must
    -- not hand out live sessions. A fast hash is correct here (unlike for
    -- passwords) because the token is 256 bits of CSPRNG output - there is no
    -- dictionary to run against it.
    token_hash bytea NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    -- NULL while the token is live. Set by logout and by refresh rotation.
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Logout-all and the expiry sweep both scan by user; the partial index keeps
-- that scan to the live tokens, which is the small set.
CREATE INDEX refresh_tokens_live_by_user_idx
    ON refresh_tokens (user_id) WHERE revoked_at IS NULL;
