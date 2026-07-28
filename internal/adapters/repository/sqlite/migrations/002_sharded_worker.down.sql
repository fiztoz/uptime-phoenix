-- Revert sharded worker support.
-- Created_at: 2026-06-27
-- Updated_at: 2026-06-27

DROP INDEX IF EXISTS idx_monitors_lease;
-- SQLite does not support DROP COLUMN in older versions; columns are harmless.
