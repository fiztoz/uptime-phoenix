-- Effective membership and policy snapshots. Upgrade with all writers stopped.
-- Old revisions were not retained: seed only the current set from its last edit.
CREATE TABLE IF NOT EXISTS monitor_probe_assignment_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    probe_id TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK (typeof(generation) = 'integer' AND generation >= 1),
    revision INTEGER NOT NULL CHECK (typeof(revision) = 'integer' AND revision >= 1),
    health_policy TEXT NOT NULL CHECK (health_policy IN ('any_down', 'all_down')),
    started_at TIMESTAMP NOT NULL,
    ended_at TIMESTAMP,
    CHECK (ended_at IS NULL OR ended_at > started_at),
    UNIQUE (monitor_id, revision, probe_id)
);
CREATE INDEX IF NOT EXISTS idx_probe_assignment_history_time
    ON monitor_probe_assignment_history (monitor_id, started_at, id);
CREATE INDEX IF NOT EXISTS idx_probe_assignment_history_open
    ON monitor_probe_assignment_history (monitor_id, ended_at);

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
