-- Run only with all application processes stopped; MariaDB DDL auto-commits.
-- A failed guard is intentional: remote heartbeat or rollup rows would be
-- collapsed by restoring UNIQUE (monitor_id, bucket).

CREATE TABLE IF NOT EXISTS probe_heartbeat_down_guard (
    state_count BIGINT NOT NULL CHECK (state_count = 0)
) ENGINE=InnoDB;
DELETE FROM probe_heartbeat_down_guard;
INSERT INTO probe_heartbeat_down_guard (state_count)
SELECT (SELECT COUNT(*) FROM heartbeats WHERE probe_id <> 'local' OR stream_id IS NOT NULL)
     + (SELECT COUNT(*) FROM heartbeat_1m WHERE probe_id <> 'local')
     + (SELECT COUNT(*) FROM heartbeat_1h WHERE probe_id <> 'local')
     + (SELECT COUNT(*) FROM heartbeat_1d WHERE probe_id <> 'local');

ALTER TABLE heartbeats DROP INDEX idx_hb_monitor_probe_time;
ALTER TABLE heartbeats
    DROP COLUMN probe_id,
    DROP COLUMN stream_id,
    DROP COLUMN source_seq,
    DROP COLUMN assignment_generation,
    DROP COLUMN received_at,
    DROP COLUMN config_revision;

ALTER TABLE heartbeat_1m DROP INDEX idx_1m_monitor_probe_bucket;
ALTER TABLE heartbeat_1m DROP INDEX uq_monitor_probe_bucket;
ALTER TABLE heartbeat_1m ADD UNIQUE KEY uq_monitor_bucket (monitor_id, bucket);
ALTER TABLE heartbeat_1m DROP COLUMN probe_id, DROP COLUMN unknown_count;

ALTER TABLE heartbeat_1h DROP INDEX idx_1h_monitor_probe_bucket;
ALTER TABLE heartbeat_1h DROP INDEX uq_monitor_probe_bucket;
ALTER TABLE heartbeat_1h ADD UNIQUE KEY uq_monitor_bucket (monitor_id, bucket);
ALTER TABLE heartbeat_1h DROP COLUMN probe_id, DROP COLUMN unknown_count;

ALTER TABLE heartbeat_1d DROP INDEX idx_1d_monitor_probe_bucket;
ALTER TABLE heartbeat_1d DROP INDEX uq_monitor_probe_bucket;
ALTER TABLE heartbeat_1d ADD UNIQUE KEY uq_monitor_bucket (monitor_id, bucket);
ALTER TABLE heartbeat_1d DROP COLUMN probe_id, DROP COLUMN unknown_count;

DROP TABLE probe_heartbeat_down_guard;
