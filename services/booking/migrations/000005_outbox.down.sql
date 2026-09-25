-- Unpublished events are dropped with the table. Rolling back loses every event
-- still owed to the broker, so let the relay drain it first
-- (SELECT count(*) FROM outbox WHERE published_at IS NULL).
DROP INDEX IF EXISTS reservations_lapsed_idx;
DROP TABLE IF EXISTS outbox;
