CREATE TABLE IF NOT EXISTS probe_connections (
    probe_id TEXT PRIMARY KEY REFERENCES probes(id) ON DELETE RESTRICT,
    hub_id TEXT NOT NULL,
    stream_id TEXT NOT NULL UNIQUE,
    enrollment_id TEXT NOT NULL UNIQUE,
    credential_version INTEGER NOT NULL CHECK (credential_version > 0),
    endpoint TEXT NOT NULL,
    fingerprint TEXT NOT NULL CHECK (length(fingerprint) = 64),
    protected_credential BLOB NOT NULL CHECK (length(protected_credential) BETWEEN 30 AND 300),
    state TEXT NOT NULL CHECK (state IN ('prepared', 'active')),
    prepared_at TIMESTAMP NOT NULL,
    activated_at TIMESTAMP,
    CHECK ((state = 'prepared' AND activated_at IS NULL) OR (state = 'active' AND activated_at IS NOT NULL))
);
