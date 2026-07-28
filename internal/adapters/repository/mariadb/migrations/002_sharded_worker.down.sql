-- Revert sharded worker support.
-- Created_at: 2026-06-27
-- Updated_at: 2026-06-27

ALTER TABLE monitors DROP INDEX idx_monitors_lease;
ALTER TABLE monitors DROP COLUMN leased_at;
ALTER TABLE monitors DROP COLUMN worker_id;
