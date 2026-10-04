-- Materialized overall health projections and historical intervals.
-- Regional samples stay in probe_observations; these rows are the overall read model.

CREATE TABLE IF NOT EXISTS monitor_health_state (
    monitor_id INTEGER NOT NULL PRIMARY KEY,
    health_policy TEXT NOT NULL CHECK (health_policy IN ('any_down', 'all_down')),
    projection_version INTEGER NOT NULL CHECK (typeof(projection_version) = 'integer' AND projection_version >= 1),
    status INTEGER NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    assigned_count INTEGER NOT NULL,
    up_count INTEGER NOT NULL,
    down_count INTEGER NOT NULL,
    pending_count INTEGER NOT NULL,
    unknown_count INTEGER NOT NULL,
    maintenance_count INTEGER NOT NULL,
    paused_count INTEGER NOT NULL,
    last_transition_at TEXT NOT NULL,
    as_of TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS monitor_health_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id INTEGER NOT NULL,
    started_at TEXT NOT NULL,
    ended_at TEXT,
    status INTEGER NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    cause TEXT NOT NULL CHECK (cause IN ('regional', 'freshness', 'assignment', 'policy', 'administrative')),
    policy_revision INTEGER NOT NULL CHECK (typeof(policy_revision) = 'integer' AND policy_revision >= 0),
    health_policy TEXT NOT NULL CHECK (health_policy IN ('any_down', 'all_down')),
    projection_version INTEGER NOT NULL CHECK (typeof(projection_version) = 'integer' AND projection_version >= 0),
    assigned_count INTEGER NOT NULL,
    up_count INTEGER NOT NULL,
    down_count INTEGER NOT NULL,
    pending_count INTEGER NOT NULL,
    unknown_count INTEGER NOT NULL,
    maintenance_count INTEGER NOT NULL,
    paused_count INTEGER NOT NULL,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_monitor_health_history_time
    ON monitor_health_history (monitor_id, started_at, id);

CREATE INDEX IF NOT EXISTS idx_probe_dirty_resolution
    ON probe_dirty_buckets (resolution, bucket, monitor_id);
