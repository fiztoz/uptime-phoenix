-- Effective membership and policy snapshots. Upgrade with all writers stopped.
-- Old revisions were not retained: seed only the current set from its last edit.
CREATE TABLE IF NOT EXISTS monitor_probe_assignment_history (
    id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    monitor_id BIGINT NOT NULL,
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    generation BIGINT NOT NULL CHECK (generation >= 1),
    revision BIGINT NOT NULL CHECK (revision >= 1),
    health_policy VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (health_policy IN ('any_down', 'all_down')),
    started_at DATETIME(6) NOT NULL,
    ended_at DATETIME(6) NULL,
    CHECK (ended_at IS NULL OR ended_at > started_at),
    UNIQUE KEY uq_probe_assignment_history (monitor_id, revision, probe_id),
    INDEX idx_probe_assignment_history_time (monitor_id, started_at, id),
    INDEX idx_probe_assignment_history_open (monitor_id, ended_at),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
) ENGINE=InnoDB;

INSERT INTO monitor_probe_assignment_history
    (monitor_id, probe_id, generation, revision, health_policy, started_at)
SELECT s.monitor_id, a.probe_id, a.generation, s.revision, s.health_policy, s.updated_at
FROM monitor_probe_assignment_sets s
JOIN monitor_probe_assignments a ON a.monitor_id = s.monitor_id AND a.active = 1
WHERE NOT EXISTS (
    SELECT 1 FROM monitor_probe_assignment_history h WHERE h.monitor_id = s.monitor_id
);

-- Derived rows were computed without assignment history. Rebuild them on demand
-- from retained observations rather than keeping a potentially incorrect answer.
DELETE FROM monitor_health_history;
