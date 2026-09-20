CREATE TABLE edge_gaps (
    from_seq INTEGER PRIMARY KEY CHECK (typeof(from_seq) = 'integer' AND from_seq > 0),
    through_seq INTEGER NOT NULL CHECK (typeof(through_seq) = 'integer' AND through_seq >= from_seq),
    reason TEXT NOT NULL CHECK (reason IN ('retention_bytes', 'retention_age', 'disk_pressure', 'restore_loss')),
    observed_from INTEGER NOT NULL,
    observed_through INTEGER NOT NULL CHECK (observed_through >= observed_from)
);
CREATE INDEX idx_edge_telemetry_age ON edge_telemetry_outbox (observed_at, seq);
