-- Probe-reported durable queue for remote probes (issue #157). See the MariaDB
-- migration of the same number for the full rationale.
ALTER TABLE probe_sessions ADD COLUMN reported_queue_bytes INTEGER NULL CHECK (reported_queue_bytes IS NULL OR reported_queue_bytes >= 0);
ALTER TABLE probe_sessions ADD COLUMN reported_oldest_queued_at INTEGER NULL CHECK (reported_oldest_queued_at IS NULL OR reported_oldest_queued_at > 0);
ALTER TABLE probe_sessions ADD COLUMN queue_reported_at INTEGER NULL CHECK (queue_reported_at IS NULL OR queue_reported_at > 0);
