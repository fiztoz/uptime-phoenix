CREATE TABLE IF NOT EXISTS probe_sessions (
    probe_id TEXT PRIMARY KEY CHECK (probe_id <> 'local'),
    owner_id TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK (typeof(generation) = 'integer' AND generation >= 1),
    lease_until INTEGER NOT NULL CHECK (typeof(lease_until) = 'integer' AND lease_until >= 0),
    connected INTEGER NOT NULL CHECK (connected IN (0, 1)),
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);
