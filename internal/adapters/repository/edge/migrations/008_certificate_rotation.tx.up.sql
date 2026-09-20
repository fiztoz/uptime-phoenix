ALTER TABLE edge_applied_commands ADD COLUMN certificate_version INTEGER
    CHECK (certificate_version IS NULL OR (typeof(certificate_version) = 'integer' AND certificate_version > 1
        AND kind = 'certificate.prepare' AND status IN ('applied', 'already_applied')));
ALTER TABLE edge_applied_commands ADD COLUMN certificate_fingerprint TEXT
    CHECK (certificate_fingerprint IS NULL OR (length(certificate_fingerprint) = 64 AND certificate_version IS NOT NULL));
ALTER TABLE edge_applied_commands ADD COLUMN certificate_not_after INTEGER
    CHECK (certificate_not_after IS NULL OR (typeof(certificate_not_after) = 'integer' AND certificate_version IS NOT NULL));

CREATE TABLE edge_certificate_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    active_version INTEGER NOT NULL CHECK (typeof(active_version) = 'integer' AND active_version >= 1),
    highest_version INTEGER NOT NULL CHECK (typeof(highest_version) = 'integer' AND highest_version >= active_version)
);
INSERT INTO edge_certificate_state VALUES (1, 1, 1);

CREATE TABLE edge_certificate_rotations (
    rotation_id TEXT PRIMARY KEY,
    version INTEGER NOT NULL UNIQUE CHECK (typeof(version) = 'integer' AND version > 1),
    hub_id TEXT NOT NULL,
    probe_id TEXT NOT NULL,
    stream_id TEXT NOT NULL,
    fingerprint TEXT NOT NULL CHECK (length(fingerprint) = 64),
    created_at INTEGER NOT NULL,
    not_before INTEGER NOT NULL,
    not_after INTEGER NOT NULL CHECK (not_after > not_before),
    protected_pem BLOB NOT NULL CHECK (length(protected_pem) BETWEEN 30 AND 16413),
    valid_for_days INTEGER NOT NULL CHECK (valid_for_days BETWEEN 1 AND 3650),
    overlap_expires_at INTEGER NOT NULL,
    prepared_at INTEGER NOT NULL,
    activated_at INTEGER,
    overlap_closed INTEGER NOT NULL DEFAULT 0 CHECK (overlap_closed IN (0, 1)),
    previous_version INTEGER NOT NULL CHECK (typeof(previous_version) = 'integer' AND previous_version >= 1 AND previous_version < version),
    retain_until INTEGER NOT NULL,
    CHECK (overlap_expires_at > prepared_at),
    CHECK (activated_at IS NULL OR (activated_at >= prepared_at AND activated_at < overlap_expires_at))
);
CREATE INDEX idx_edge_certificate_retention ON edge_certificate_rotations (retain_until, version);
