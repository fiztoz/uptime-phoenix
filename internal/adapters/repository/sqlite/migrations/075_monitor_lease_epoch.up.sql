-- Worker lease fencing (issue #63): a monitor lease is now a lease instance.
-- See the MariaDB migration of the same number for the full rationale.
ALTER TABLE monitors ADD COLUMN lease_epoch INTEGER NOT NULL DEFAULT 0;
