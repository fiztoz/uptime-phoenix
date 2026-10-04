ALTER TABLE probes ADD COLUMN revoked_at TIMESTAMP NULL;
ALTER TABLE probes ADD COLUMN deleted_at TIMESTAMP NULL;
CREATE TABLE probe_revocations (
    operation_id TEXT NOT NULL PRIMARY KEY CHECK (length(operation_id) = 36),
    probe_id TEXT NOT NULL UNIQUE REFERENCES probes(id) ON DELETE RESTRICT CHECK (probe_id <> 'local'),
    created_at TIMESTAMP NOT NULL
);
