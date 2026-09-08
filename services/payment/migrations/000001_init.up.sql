-- P2 schema for the Payment service (ARCHITECTURE.md §8, payment_db).
--
-- No outbox table yet. Payment publishes no events until the broker arrives in
-- P4; an outbox with no relay is scaffolding, and AGENTS.md §2 rule 8 is about
-- how events are published, not a requirement to publish them early. The table
-- lands in the migration that introduces the relay.

CREATE TABLE charges (
    id             uuid PRIMARY KEY,
    -- No FK: reservations live in booking_db (D2, no shared schemas). Payment
    -- treats the reservation ID as an opaque correlation value.
    reservation_id uuid   NOT NULL,
    user_id        uuid   NOT NULL,
    -- Integer cents. Money is never a float (AGENTS.md §5).
    amount_cents   bigint NOT NULL CHECK (amount_cents > 0),
    -- 'pending' is written BEFORE the provider is called (FR-4.5), so a crash
    -- mid-call leaves a row that says "a charge was started and its outcome is
    -- unknown" rather than no evidence at all.
    status         text   NOT NULL CHECK (status IN ('pending', 'succeeded', 'declined', 'failed')),
    provider_ref   text,
    decline_reason text,
    -- The whole anti-double-charge mechanism (FR-4.3). UNIQUE is what makes a
    -- retry collide with the first attempt instead of taking the money twice;
    -- without it the application check is a check-then-act race.
    idempotency_key text  NOT NULL UNIQUE,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    -- A settled charge carries its provider reference; an unsettled one cannot.
    CONSTRAINT charges_settled_has_ref CHECK (
        (status = 'succeeded' AND provider_ref IS NOT NULL)
        OR status <> 'succeeded'
    )
);

-- Recovery scan: charges whose outcome was never resolved (FR-4.5).
CREATE INDEX charges_pending_idx ON charges (created_at) WHERE status = 'pending';
-- Booking asks "was this reservation charged?" during reconciliation.
CREATE INDEX charges_reservation_idx ON charges (reservation_id);

CREATE TABLE refunds (
    id           uuid PRIMARY KEY,
    -- A refund always references the charge it reverses (FR-4.6).
    charge_id    uuid   NOT NULL REFERENCES charges (id),
    amount_cents bigint NOT NULL CHECK (amount_cents > 0),
    status       text   NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed')),
    provider_ref text,
    idempotency_key text NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX refunds_charge_idx ON refunds (charge_id);
