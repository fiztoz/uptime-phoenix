-- Durable step-zero marker for the legacy in-process alert path. See the
-- MariaDB migration of the same number for the full rationale.
ALTER TABLE alerts ADD COLUMN initial_notified_at TIMESTAMP NULL;

UPDATE alerts SET initial_notified_at = fired_at WHERE initial_notified_at IS NULL;
