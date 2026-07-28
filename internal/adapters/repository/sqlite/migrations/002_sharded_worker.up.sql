-- Sharded worker support: add lease columns to monitors table.
-- Created_at: 2026-06-27
-- Updated_at: 2026-06-27

ALTER TABLE monitors ADD COLUMN worker_id TEXT DEFAULT NULL;
ALTER TABLE monitors ADD COLUMN leased_at TIMESTAMP NULL DEFAULT NULL;

CREATE INDEX idx_monitors_lease ON monitors (active, worker_id, leased_at);
