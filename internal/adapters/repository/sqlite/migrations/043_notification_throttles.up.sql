-- Stop all writers for upgrade. Old in-memory attempt times cannot be recovered.
CREATE TABLE IF NOT EXISTS notification_throttles (
    monitor_id INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    probe_id TEXT NOT NULL REFERENCES probes(id) ON DELETE CASCADE,
    assignment_generation INTEGER NOT NULL CHECK (assignment_generation > 0),
    last_attempt_at TIMESTAMP,
    PRIMARY KEY (monitor_id, probe_id, assignment_generation)
);
