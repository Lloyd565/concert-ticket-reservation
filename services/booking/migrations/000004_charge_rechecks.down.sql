-- Queued rechecks are dropped with the table. Rolling back loses the promise to
-- look at those charge keys again, so drain the queue first (let reconciliation
-- run past the grace period) if any rows remain.
DROP TABLE IF EXISTS charge_rechecks;
