-- Revert the probe-reported queue. An older binary never reads or writes these
-- columns; the probe detail falls back to "not tracked by the hub".
ALTER TABLE probe_sessions DROP COLUMN queue_reported_at;
ALTER TABLE probe_sessions DROP COLUMN reported_oldest_queued_at;
ALTER TABLE probe_sessions DROP COLUMN reported_queue_bytes;
