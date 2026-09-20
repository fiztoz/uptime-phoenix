ALTER TABLE edge_applied_commands ADD COLUMN credential_version INTEGER
    CHECK (credential_version IS NULL OR
        (typeof(credential_version) = 'integer' AND credential_version > 0 AND
         kind = 'credential.prepare' AND status IN ('applied', 'already_applied')));

-- The high-water remains even after bounded receipt/rotation cleanup.
CREATE TABLE edge_credential_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    highest_version INTEGER NOT NULL CHECK (typeof(highest_version) = 'integer' AND highest_version >= 0)
);
INSERT INTO edge_credential_state VALUES (1, COALESCE((SELECT version FROM edge_credentials WHERE kind = 'runtime'), 0));

CREATE TABLE edge_credential_rotations (
    rotation_id TEXT PRIMARY KEY,
    version INTEGER NOT NULL UNIQUE CHECK (typeof(version) = 'integer' AND version > 0),
    token_hash BLOB NOT NULL CHECK (length(token_hash) = 32),
    overlap_expires_at INTEGER NOT NULL,
    prepared_at INTEGER NOT NULL,
    activated_at INTEGER,
    overlap_closed INTEGER NOT NULL DEFAULT 0 CHECK (overlap_closed IN (0, 1)),
    previous_token_hash BLOB NOT NULL CHECK (length(previous_token_hash) = 32),
    previous_version INTEGER NOT NULL CHECK (typeof(previous_version) = 'integer' AND previous_version > 0 AND previous_version < version),
    previous_issued_at INTEGER NOT NULL,
    retain_until INTEGER NOT NULL,
    CHECK (overlap_expires_at > prepared_at),
    CHECK (activated_at IS NULL OR activated_at < overlap_expires_at)
);
CREATE INDEX idx_edge_rotation_retention ON edge_credential_rotations (retain_until, version);
