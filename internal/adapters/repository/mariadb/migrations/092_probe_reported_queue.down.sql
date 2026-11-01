-- Revert the probe-reported queue. An older binary never reads or writes these
-- columns; the probe detail falls back to "not tracked by the hub".
ALTER TABLE probe_sessions
    DROP COLUMN IF EXISTS queue_reported_at,
    DROP COLUMN IF EXISTS reported_oldest_queued_at,
    DROP COLUMN IF EXISTS reported_queue_bytes;
