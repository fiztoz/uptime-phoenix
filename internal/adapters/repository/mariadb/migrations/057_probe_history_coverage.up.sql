ALTER TABLE probe_dirty_buckets ADD COLUMN revision VARCHAR(36) NOT NULL DEFAULT '00000000-0000-4000-8000-000000000000';
INSERT IGNORE INTO probe_dirty_buckets (monitor_id,probe_id,resolution,bucket) SELECT monitor_id,'local','overall',bucket FROM probe_dirty_buckets WHERE resolution = 'overall' GROUP BY monitor_id,bucket;
DELETE FROM probe_dirty_buckets WHERE resolution = 'overall' AND probe_id <> 'local';
UPDATE probe_dirty_buckets SET revision = UUID();
ALTER TABLE heartbeat_1m ADD COLUMN up_us BIGINT NOT NULL DEFAULT 0 CHECK (up_us >= 0);
ALTER TABLE heartbeat_1m ADD COLUMN down_us BIGINT NOT NULL DEFAULT 0 CHECK (down_us >= 0);
ALTER TABLE heartbeat_1m ADD COLUMN pending_us BIGINT NOT NULL DEFAULT 0 CHECK (pending_us >= 0);
ALTER TABLE heartbeat_1m ADD COLUMN unknown_us BIGINT NOT NULL DEFAULT 0 CHECK (unknown_us >= 0);
ALTER TABLE heartbeat_1m ADD COLUMN maintenance_us BIGINT NOT NULL DEFAULT 0 CHECK (maintenance_us >= 0);
ALTER TABLE heartbeat_1m ADD COLUMN paused_us BIGINT NOT NULL DEFAULT 0 CHECK (paused_us >= 0);
ALTER TABLE heartbeat_1m ADD COLUMN history_managed BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE heartbeat_1h ADD COLUMN up_us BIGINT NOT NULL DEFAULT 0 CHECK (up_us >= 0);
ALTER TABLE heartbeat_1h ADD COLUMN down_us BIGINT NOT NULL DEFAULT 0 CHECK (down_us >= 0);
ALTER TABLE heartbeat_1h ADD COLUMN pending_us BIGINT NOT NULL DEFAULT 0 CHECK (pending_us >= 0);
ALTER TABLE heartbeat_1h ADD COLUMN unknown_us BIGINT NOT NULL DEFAULT 0 CHECK (unknown_us >= 0);
ALTER TABLE heartbeat_1h ADD COLUMN maintenance_us BIGINT NOT NULL DEFAULT 0 CHECK (maintenance_us >= 0);
ALTER TABLE heartbeat_1h ADD COLUMN paused_us BIGINT NOT NULL DEFAULT 0 CHECK (paused_us >= 0);
ALTER TABLE heartbeat_1h ADD COLUMN history_managed BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE heartbeat_1d ADD COLUMN up_us BIGINT NOT NULL DEFAULT 0 CHECK (up_us >= 0);
ALTER TABLE heartbeat_1d ADD COLUMN down_us BIGINT NOT NULL DEFAULT 0 CHECK (down_us >= 0);
ALTER TABLE heartbeat_1d ADD COLUMN pending_us BIGINT NOT NULL DEFAULT 0 CHECK (pending_us >= 0);
ALTER TABLE heartbeat_1d ADD COLUMN unknown_us BIGINT NOT NULL DEFAULT 0 CHECK (unknown_us >= 0);
ALTER TABLE heartbeat_1d ADD COLUMN maintenance_us BIGINT NOT NULL DEFAULT 0 CHECK (maintenance_us >= 0);
ALTER TABLE heartbeat_1d ADD COLUMN paused_us BIGINT NOT NULL DEFAULT 0 CHECK (paused_us >= 0);
ALTER TABLE heartbeat_1d ADD COLUMN history_managed BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX idx_probe_observations_history_seed ON probe_observations (monitor_id, probe_id, assignment_generation, stream_id, seq);

CREATE TABLE probe_history_ranges (
    monitor_id BIGINT NOT NULL,
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    cursor_at DATETIME(6) NOT NULL,
    observed_through DATETIME(6) NOT NULL,
    PRIMARY KEY (monitor_id, probe_id),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;
CREATE INDEX idx_probe_history_ranges_cursor ON probe_history_ranges (cursor_at, monitor_id, probe_id);
CREATE INDEX idx_probe_observations_history_time ON probe_observations (monitor_id, probe_id, assignment_generation, stream_id, observed_at);
