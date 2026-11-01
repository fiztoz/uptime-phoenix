-- Probe-reported durable queue for remote probes (issue #157).
--
-- Every authenticated probe health frame carries the probe's own replay queue:
-- queue_bytes (>= 0) and oldest_queued_at (NULL exactly when the queue is
-- empty). These columns keep the last report the owning hub accepted:
--   reported_queue_bytes       bytes the probe said it had queued;
--   reported_oldest_queued_at  the probe's oldest queued item, Unix
--                              microseconds on the probe's clock (UTC);
--   queue_reported_at          the hub database clock (Unix seconds, same
--                              clock as last_contact_at) at acceptance.
-- They are written in the same fenced statement as last_contact_at (091), so
-- only the live session generation's owner can write them and they never move
-- backward. Release and takeover keep them, so they freeze while the probe is
-- disconnected. The values are self-reported display evidence, never
-- hub-verified backlog and never an input to readiness or alerting.
--
-- NULL means the probe has not reported since this migration; existing rows are
-- not backfilled. ADD COLUMN IF NOT EXISTS keeps the script idempotent.
ALTER TABLE probe_sessions
    ADD COLUMN IF NOT EXISTS reported_queue_bytes BIGINT NULL CHECK (reported_queue_bytes IS NULL OR reported_queue_bytes >= 0),
    ADD COLUMN IF NOT EXISTS reported_oldest_queued_at BIGINT NULL CHECK (reported_oldest_queued_at IS NULL OR reported_oldest_queued_at > 0),
    ADD COLUMN IF NOT EXISTS queue_reported_at BIGINT NULL CHECK (queue_reported_at IS NULL OR queue_reported_at > 0);
