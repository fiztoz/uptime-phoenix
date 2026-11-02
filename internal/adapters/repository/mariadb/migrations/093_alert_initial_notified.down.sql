-- Revert the step-zero marker. An older binary never reads or writes it.
ALTER TABLE alerts DROP COLUMN IF EXISTS initial_notified_at;
