-- Durable administrative operations and the frozen registration network trust.
-- Operations are persisted before any 202 is returned: an operation row is the
-- only proof that accepted work exists. Endpoint/pin live on the registration
-- (identity, immutable after create); enrollment copies them into the prepared
-- connection. Neither is authentication material.
CREATE TABLE IF NOT EXISTS probe_operations (
    operation_id TEXT NOT NULL PRIMARY KEY CHECK (length(operation_id) = 36),
    probe_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('enroll', 'rotate_credential', 'reset_stream')),
    status TEXT NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed')),
    phase TEXT NOT NULL CHECK (length(phase) BETWEEN 1 AND 128 AND phase NOT GLOB '*[^a-z0-9_]*'),
    error_code TEXT NULL,
    error_message TEXT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK ((status = 'failed' AND error_code IS NOT NULL AND error_message IS NOT NULL) OR
           (status <> 'failed' AND error_code IS NULL AND error_message IS NULL)),
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);

ALTER TABLE probes ADD COLUMN endpoint TEXT NULL;
ALTER TABLE probes ADD COLUMN tls_fingerprint TEXT NULL;
