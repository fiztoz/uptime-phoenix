-- Durable step-zero marker for the legacy in-process alert path.
--
-- The legacy dispatcher opens the alert and then calls the providers outside
-- any transaction. A process killed between the two left the alert open with
-- no notification sent, and the restarted hub saw DOWN after DOWN and never
-- sent it. initial_notified_at records the first step-zero attempt; an open,
-- firing alert with NULL here is sent on the next DOWN check (at-least-once).
--
-- Existing rows are backfilled from fired_at so an upgrade never re-sends an
-- alert that the previous binary already delivered.
-- ADD COLUMN IF NOT EXISTS keeps the script idempotent (068 tail-heal idiom).
ALTER TABLE alerts
    ADD COLUMN IF NOT EXISTS initial_notified_at TIMESTAMP NULL DEFAULT NULL;

UPDATE alerts SET initial_notified_at = fired_at, updated_at = updated_at WHERE initial_notified_at IS NULL;
