-- Stop all writers for upgrade. Old in-memory attempt times cannot be recovered.
CREATE TABLE IF NOT EXISTS notification_throttles (
    monitor_id BIGINT NOT NULL,
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    assignment_generation BIGINT NOT NULL CHECK (assignment_generation > 0),
    last_attempt_at DATETIME(6) NULL,
    PRIMARY KEY (monitor_id, probe_id, assignment_generation),
    CONSTRAINT fk_notification_throttle_monitor FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    CONSTRAINT fk_notification_throttle_probe FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE CASCADE
) ENGINE=InnoDB;
