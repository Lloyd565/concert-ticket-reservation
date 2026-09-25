-- Unpublished events are dropped with the table: let the relay drain it first
-- (SELECT count(*) FROM outbox WHERE published_at IS NULL).
DROP TABLE IF EXISTS outbox;
