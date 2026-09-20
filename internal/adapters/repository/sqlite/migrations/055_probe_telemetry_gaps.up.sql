CREATE TABLE probe_telemetry_gaps (
    probe_id TEXT NOT NULL,
    stream_id TEXT NOT NULL,
    from_seq INTEGER NOT NULL CHECK (from_seq > 0),
    through_seq INTEGER NOT NULL CHECK (through_seq >= from_seq),
    reason VARCHAR(32) NOT NULL,
    observed_from TIMESTAMP NOT NULL,
    observed_through TIMESTAMP NOT NULL,
    affected_monitor_ids TEXT NOT NULL,
    received_at TIMESTAMP NOT NULL,
    recompute_pending BOOLEAN NOT NULL DEFAULT TRUE,
    recompute_monitor_id INTEGER NOT NULL DEFAULT 0,
    recompute_bucket TIMESTAMP NOT NULL,
    PRIMARY KEY (probe_id, stream_id, from_seq),
    FOREIGN KEY (probe_id, stream_id) REFERENCES probe_streams(probe_id, stream_id) ON DELETE RESTRICT
);
CREATE INDEX idx_probe_gap_work ON probe_telemetry_gaps (recompute_pending, received_at);
