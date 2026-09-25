-- P4 schema for the Notification service (ARCHITECTURE.md §8, notif_db).
--
-- Everything here serves one promise: an event produces at most one email, even
-- though every event can be delivered more than once (D10).

CREATE TABLE notifications (
    -- The event_id this notification was produced from. One event, one
    -- notification: a redelivered event resumes this row - and its attempt
    -- count - rather than starting a second notification.
    -- ponytail: one notification per event. An event that fans out to several
    -- channels needs (event_id, channel) instead.
    id         uuid PRIMARY KEY,
    -- No FK: users live in auth_db (D2, no shared schemas).
    user_id    uuid        NOT NULL,
    -- The event type that produced it.
    type       text        NOT NULL,
    channel    text        NOT NULL CHECK (channel IN ('email')),
    status     text        NOT NULL CHECK (status IN ('pending', 'sent', 'failed')),
    -- The message as rendered: what was (or was going to be) sent.
    payload    jsonb       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX notifications_user_idx ON notifications (user_id);

-- The delivery log: one row per attempt, successful or not.
CREATE TABLE delivery_attempts (
    id              uuid        PRIMARY KEY,
    notification_id uuid        NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
    attempt_no      integer     NOT NULL CHECK (attempt_no > 0),
    status          text        NOT NULL CHECK (status IN ('sent', 'failed')),
    error           text,
    attempted_at    timestamptz NOT NULL DEFAULT now(),

    UNIQUE (notification_id, attempt_no)
);

-- Consumer idempotency (D10). A row here means the event's notification has been
-- sent, and it is written by the same statement that records the send. Every
-- message is checked against this table before anything else happens to it.
CREATE TABLE processed_events (
    event_id     uuid        PRIMARY KEY,
    processed_at timestamptz NOT NULL DEFAULT now()
);
