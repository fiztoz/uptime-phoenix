CREATE TABLE IF NOT EXISTS probe_runtime_owners (
    probe_id TEXT PRIMARY KEY CHECK (probe_id <> 'local'),
    owner_id TEXT NOT NULL,
    epoch INTEGER NOT NULL CHECK (typeof(epoch) = 'integer' AND epoch >= 1),
    lease_until INTEGER NOT NULL CHECK (typeof(lease_until) = 'integer' AND lease_until >= 0),
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);
