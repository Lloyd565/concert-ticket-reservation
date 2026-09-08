DROP TABLE IF EXISTS tickets;
DROP TABLE IF EXISTS bookings;
DROP INDEX IF EXISTS reservations_awaiting_reconciliation_idx;
ALTER TABLE reservations DROP COLUMN IF EXISTS payment_pending_since;
ALTER TABLE reservations DROP COLUMN IF EXISTS total_cents;
ALTER TABLE seats DROP COLUMN IF EXISTS price_cents;
