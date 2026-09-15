-- Regional heartbeat identity and per-probe rollup uniqueness.
-- Keep PRIMARY KEY (id, time) and PARTITION BY RANGE (UNIX_TIMESTAMP(time)).
-- Do not add UNIQUE (stream_id, seq): a unique key that omits the partition
-- expression is illegal on this table. Dedup lives in probe_streams.
-- Adding columns/indexes on a populated partitioned table is not instant;
-- measure lock and disk cost on real MariaDB before production rollout.

ALTER TABLE heartbeats
    ADD COLUMN probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'local',
    ADD COLUMN stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ADD COLUMN source_seq BIGINT NULL,
    ADD COLUMN assignment_generation BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN received_at DATETIME(6) NULL,
    ADD COLUMN config_revision BIGINT NOT NULL DEFAULT 0;

CREATE INDEX idx_hb_monitor_probe_time ON heartbeats (monitor_id, probe_id, time DESC, id DESC);

ALTER TABLE heartbeat_1m
    ADD COLUMN probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'local' AFTER monitor_id,
    ADD COLUMN unknown_count INT NOT NULL DEFAULT 0 AFTER maint_count;
ALTER TABLE heartbeat_1m ADD UNIQUE KEY uq_monitor_probe_bucket (monitor_id, probe_id, bucket);
ALTER TABLE heartbeat_1m DROP INDEX uq_monitor_bucket;
ALTER TABLE heartbeat_1m ADD INDEX idx_1m_monitor_probe_bucket (monitor_id, probe_id, bucket DESC);

ALTER TABLE heartbeat_1h
    ADD COLUMN probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'local' AFTER monitor_id,
    ADD COLUMN unknown_count INT NOT NULL DEFAULT 0 AFTER maint_count;
ALTER TABLE heartbeat_1h ADD UNIQUE KEY uq_monitor_probe_bucket (monitor_id, probe_id, bucket);
ALTER TABLE heartbeat_1h DROP INDEX uq_monitor_bucket;
ALTER TABLE heartbeat_1h ADD INDEX idx_1h_monitor_probe_bucket (monitor_id, probe_id, bucket DESC);

ALTER TABLE heartbeat_1d
    ADD COLUMN probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'local' AFTER monitor_id,
    ADD COLUMN unknown_count INT NOT NULL DEFAULT 0 AFTER maint_count;
ALTER TABLE heartbeat_1d ADD UNIQUE KEY uq_monitor_probe_bucket (monitor_id, probe_id, bucket);
ALTER TABLE heartbeat_1d DROP INDEX uq_monitor_bucket;
ALTER TABLE heartbeat_1d ADD INDEX idx_1d_monitor_probe_bucket (monitor_id, probe_id, bucket DESC);
