CREATE TABLE probe_telemetry_gaps (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    from_seq BIGINT NOT NULL CHECK (from_seq > 0),
    through_seq BIGINT NOT NULL CHECK (through_seq >= from_seq),
    reason VARCHAR(32) NOT NULL,
    observed_from DATETIME(6) NOT NULL,
    observed_through DATETIME(6) NOT NULL,
    affected_monitor_ids TEXT NOT NULL,
    received_at DATETIME(6) NOT NULL,
    recompute_pending BOOLEAN NOT NULL DEFAULT TRUE,
    recompute_monitor_id BIGINT NOT NULL DEFAULT 0,
    recompute_bucket DATETIME(6) NOT NULL,
    PRIMARY KEY (probe_id, stream_id, from_seq),
    FOREIGN KEY (probe_id, stream_id) REFERENCES probe_streams(probe_id, stream_id) ON DELETE RESTRICT
) ENGINE=InnoDB;
CREATE INDEX idx_probe_gap_work ON probe_telemetry_gaps (recompute_pending, received_at);
