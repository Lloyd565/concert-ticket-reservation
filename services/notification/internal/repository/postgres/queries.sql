-- Hand-written SQL compiled by sqlc into type-safe Go. No ORM by design
-- (AGENTS.md §2 rule 4).
--
-- The property that matters in this file is that "this event has been handled"
-- and "this email was sent" are recorded by one statement, so they can never
-- disagree (see MarkNotificationSent).

-- name: IsEventProcessed :one
--
-- The first question asked of every message (D10). Delivery is at-least-once, so
-- a yes here is normal, and means: acknowledge and do nothing.
SELECT EXISTS (SELECT 1 FROM processed_events WHERE event_id = @event_id);

-- name: StartNotification :one
--
-- Records the notification about to be attempted, and returns how many attempts
-- it has already had.
--
-- ON CONFLICT DO NOTHING because a redelivered event - the consumer died
-- mid-retry, or the message was replayed from the dead-letter queue - must resume
-- its notification, not start a second one. The attempt count carries over with
-- it, so a crash loop cannot retry a message forever.
WITH created AS (
    INSERT INTO notifications (id, user_id, type, channel, status, payload)
    VALUES (@id, @user_id, @type, @channel, 'pending', @payload)
    ON CONFLICT (id) DO NOTHING
)
SELECT count(*) FROM delivery_attempts WHERE notification_id = @id;

-- name: RecordFailedAttempt :exec
INSERT INTO delivery_attempts (id, notification_id, attempt_no, status, error)
VALUES (@id, @notification_id, @attempt_no, 'failed', @error);

-- name: MarkNotificationSent :exec
--
-- The attempt, the status and the processed_events row in one statement.
--
-- Written as separate steps, a crash between "sent" and "processed" would let a
-- redelivery send the email again; a crash the other way round would mark an
-- event handled whose email never went out. One statement has no in-between.
WITH attempt AS (
    INSERT INTO delivery_attempts (id, notification_id, attempt_no, status)
    VALUES (@attempt_id, @notification_id, @attempt_no, 'sent')
), sent AS (
    UPDATE notifications
    SET status = 'sent'
    WHERE id = @notification_id
    RETURNING id
)
INSERT INTO processed_events (event_id)
SELECT id FROM sent
ON CONFLICT (event_id) DO NOTHING;

-- name: MarkNotificationFailed :exec
--
-- Out of attempts; the message goes to the dead-letter queue. The event is NOT
-- recorded as processed: replaying it from the DLQ once delivery works again
-- must send the email.
UPDATE notifications
SET status = 'failed'
WHERE id = @id
  AND status = 'pending';
