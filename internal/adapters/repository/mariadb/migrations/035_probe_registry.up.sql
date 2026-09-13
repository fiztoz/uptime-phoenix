-- Registry/assignment foundation only. Existing scheduling does not read these
-- tables. New monitor creation must be wired before remote activation is allowed.
CREATE TABLE IF NOT EXISTS probes (
    id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    probe_key VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
    name VARCHAR(200) NOT NULL,
    location VARCHAR(255) NOT NULL DEFAULT '',
    kind VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (kind IN ('local', 'remote')),
    enabled BOOLEAN NOT NULL CHECK (enabled IN (0, 1)),
    revision BIGINT NOT NULL CHECK (revision >= 1),
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    CHECK ((id = 'local' AND probe_key = 'local' AND kind = 'local') OR
           (id <> 'local' AND probe_key <> 'local' AND kind = 'remote'))
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS monitor_probe_assignment_sets (
    monitor_id BIGINT PRIMARY KEY,
    revision BIGINT NOT NULL CHECK (revision >= 1),
    health_policy VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (health_policy IN ('any_down', 'all_down')),
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS monitor_probe_assignments (
    monitor_id BIGINT NOT NULL,
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    generation BIGINT NOT NULL CHECK (generation >= 1),
    active BOOLEAN NOT NULL CHECK (active IN (0, 1)),
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (monitor_id, probe_id),
    INDEX idx_probe_assignments_probe_active (probe_id, active, monitor_id),
    FOREIGN KEY (monitor_id) REFERENCES monitor_probe_assignment_sets(monitor_id) ON DELETE CASCADE,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

INSERT INTO probes (id, probe_key, name, location, kind, enabled, revision, created_at, updated_at)
VALUES ('local', 'local', 'Local', '', 'local', 1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6))
ON DUPLICATE KEY UPDATE id = VALUES(id);

INSERT INTO monitor_probe_assignment_sets (monitor_id, revision, health_policy, created_at, updated_at)
SELECT id, 1, 'any_down', UTC_TIMESTAMP(6), UTC_TIMESTAMP(6) FROM monitors
ON DUPLICATE KEY UPDATE monitor_id = VALUES(monitor_id);

INSERT INTO monitor_probe_assignments (monitor_id, probe_id, generation, active, created_at, updated_at)
SELECT monitor_id, 'local', 1, 1, created_at, updated_at FROM monitor_probe_assignment_sets WHERE revision = 1
ON DUPLICATE KEY UPDATE monitor_id = VALUES(monitor_id);
