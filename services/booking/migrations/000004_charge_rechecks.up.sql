-- Reservations released on "no charge" evidence, still to be looked at once more.
--
-- Nothing in 000001-000003 is edited (AGENTS.md §5) - this migration only adds.
--
-- "Payment has no charge under this key" is true when it is said, and only then.
-- A pay retry that got past the saga's fence before the release committed can
-- still reach Payment afterwards and take the money - for a reservation that is
-- now failed, whose seats are back on sale, and which the pending-only
-- reconciliation scan will never look at again. So the release that acts on that
-- answer queues the reservation here, in its own transaction, and reconciliation
-- asks Payment again once no call started before the release can still be under
-- way. A charge found then is refunded; none found closes the entry.
--
-- A table rather than a column on reservations: a row here is work still to do,
-- and deleting it is finishing that work.
CREATE TABLE charge_rechecks (
    reservation_id uuid PRIMARY KEY REFERENCES reservations (id) ON DELETE CASCADE,
    released_at    timestamptz NOT NULL DEFAULT now()
);

-- The recheck scan, oldest first.
CREATE INDEX charge_rechecks_released_idx ON charge_rechecks (released_at);
