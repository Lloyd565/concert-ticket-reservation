-- P4: the transactional outbox (D9, ARCHITECTURE.md §4.3).
--
-- Nothing in 000001 is edited (AGENTS.md §5) - this migration only adds. It is
-- the migration 000001 promised: the table arrives with the relay that drains it.
--
-- Payment still has no TxManager. The settle UPDATE and the INSERT below are one
-- SQL statement (see SettleCharge in queries.sql), which is as atomic as a
-- transaction and cannot be split into two steps by a later edit to Go code.
CREATE TABLE outbox (
    -- Also the envelope's event_id, which is what consumers deduplicate on.
    id            uuid PRIMARY KEY,
    aggregate_id  uuid        NOT NULL,
    -- The routing key.
    event_type    text        NOT NULL,
    -- The complete envelope, exactly as it will be published.
    payload       jsonb       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- NULL until the broker has confirmed the message.
    published_at  timestamptz,
    -- A relay's lease on an unpublished row: publishing happens outside any
    -- transaction (D4), so this, not a row lock, keeps two relays apart.
    claimed_until timestamptz
);

-- The relay's scan: unpublished rows, oldest first.
CREATE INDEX outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL;
