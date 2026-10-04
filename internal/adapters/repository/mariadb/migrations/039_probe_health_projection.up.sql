-- Materialized overall health projections and historical intervals.
-- Regional samples stay in probe_observations; these rows are the overall read model.

CREATE TABLE IF NOT EXISTS monitor_health_state (
    monitor_id BIGINT NOT NULL PRIMARY KEY,
    health_policy VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    projection_version BIGINT NOT NULL CHECK (projection_version >= 1),
    status TINYINT NOT NULL,
    reason TEXT NOT NULL,
    assigned_count INT NOT NULL,
    up_count INT NOT NULL,
    down_count INT NOT NULL,
    pending_count INT NOT NULL,
    unknown_count INT NOT NULL,
    maintenance_count INT NOT NULL,
    paused_count INT NOT NULL,
    last_transition_at DATETIME(6) NOT NULL,
    as_of DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    CHECK (health_policy IN ('any_down', 'all_down')),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS monitor_health_history (
    id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    monitor_id BIGINT NOT NULL,
    started_at DATETIME(6) NOT NULL,
    ended_at DATETIME(6) NULL,
    status TINYINT NOT NULL,
    reason TEXT NOT NULL,
    cause VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    policy_revision BIGINT NOT NULL CHECK (policy_revision >= 0),
    health_policy VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    projection_version BIGINT NOT NULL CHECK (projection_version >= 0),
    assigned_count INT NOT NULL,
    up_count INT NOT NULL,
    down_count INT NOT NULL,
    pending_count INT NOT NULL,
    unknown_count INT NOT NULL,
    maintenance_count INT NOT NULL,
    paused_count INT NOT NULL,
    CHECK (health_policy IN ('any_down', 'all_down')),
    CHECK (cause IN ('regional', 'freshness', 'assignment', 'policy', 'administrative')),
    INDEX idx_monitor_health_history_time (monitor_id, started_at, id),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE INDEX idx_probe_dirty_resolution ON probe_dirty_buckets (resolution, bucket, monitor_id);
