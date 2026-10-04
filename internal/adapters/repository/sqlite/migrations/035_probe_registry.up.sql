-- Registry/assignment foundation only. Existing scheduling does not read these
-- tables. New monitor creation must be wired before remote activation is allowed.
CREATE TABLE IF NOT EXISTS probes (
    id TEXT NOT NULL PRIMARY KEY,
    probe_key TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    location TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL CHECK (kind IN ('local', 'remote')),
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    revision INTEGER NOT NULL CHECK (typeof(revision) = 'integer' AND revision >= 1),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    CHECK ((id = 'local' AND probe_key = 'local' AND kind = 'local') OR
           (id <> 'local' AND probe_key <> 'local' AND kind = 'remote'))
);

CREATE TABLE IF NOT EXISTS monitor_probe_assignment_sets (
    monitor_id INTEGER PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL CHECK (typeof(revision) = 'integer' AND revision >= 1),
    health_policy TEXT NOT NULL CHECK (health_policy IN ('any_down', 'all_down')),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS monitor_probe_assignments (
    monitor_id INTEGER NOT NULL REFERENCES monitor_probe_assignment_sets(monitor_id) ON DELETE CASCADE,
    probe_id TEXT NOT NULL REFERENCES probes(id) ON DELETE RESTRICT,
    generation INTEGER NOT NULL CHECK (typeof(generation) = 'integer' AND generation >= 1),
    active INTEGER NOT NULL CHECK (active IN (0, 1)),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (monitor_id, probe_id)
);
CREATE INDEX IF NOT EXISTS idx_probe_assignments_probe_active
    ON monitor_probe_assignments (probe_id, active, monitor_id);

INSERT INTO probes (id, probe_key, name, location, kind, enabled, revision, created_at, updated_at)
VALUES ('local', 'local', 'Local', '', 'local', 1, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
ON CONFLICT (id) DO NOTHING;

INSERT INTO monitor_probe_assignment_sets (monitor_id, revision, health_policy, created_at, updated_at)
SELECT id, 1, 'any_down', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP FROM monitors WHERE 1
ON CONFLICT (monitor_id) DO NOTHING;

INSERT INTO monitor_probe_assignments (monitor_id, probe_id, generation, active, created_at, updated_at)
SELECT monitor_id, 'local', 1, 1, created_at, updated_at FROM monitor_probe_assignment_sets WHERE revision = 1
ON CONFLICT (monitor_id, probe_id) DO NOTHING;
