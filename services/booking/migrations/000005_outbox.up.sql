-- P4: the transactional outbox (D9, ARCHITECTURE.md §4.3).
--
-- Nothing in 000001-000004 is edited (AGENTS.md §5) - this migration only adds.
--
-- A row here is an event that is owed to the broker. It is inserted in the same
-- transaction as the state change it describes, so the two commit together or
-- not at all; the relay publishes it afterwards and stamps published_at once
-- RabbitMQ has confirmed it. Writing the change and publishing the event as two
-- separate steps loses the event whenever the process dies between them.
CREATE TABLE outbox (
    -- Also the envelope's event_id, which is what consumers deduplicate on.
    id            uuid PRIMARY KEY,
    aggregate_id  uuid        NOT NULL,
    -- The routing key.
    event_type    text        NOT NULL,
    -- The complete envelope, exactly as it will be published. Built when the
    -- state change commits, not when the relay gets round to it, so a late
    -- publish still describes what happened at the time.
    payload       jsonb       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- NULL until the broker has confirmed the message.
    published_at  timestamptz,
    -- A relay's lease on an unpublished row. The relay publishes outside any
    -- transaction (D4), so a row lock cannot be what stops two Booking replicas
    -- publishing the same row; this is. A relay that dies mid-publish leaves a
    -- lease that simply lapses, and the next relay picks the row up.
    claimed_until timestamptz
);

-- The relay's scan: unpublished rows, oldest first.
CREATE INDEX outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL;

-- The expirer's scan: reservations whose checkout window closed with no payment
-- in doubt. Reservations reconciliation owns are excluded here, as they are from
-- the query that uses it (D8).
CREATE INDEX reservations_lapsed_idx ON reservations (expires_at)
    WHERE status = 'pending' AND payment_pending_since IS NULL;
