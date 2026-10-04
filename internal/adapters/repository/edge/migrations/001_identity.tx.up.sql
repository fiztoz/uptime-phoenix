-- Dedicated edge schema. This database never contains hub users or monitors.
CREATE TABLE IF NOT EXISTS edge_identity (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    probe_id TEXT NOT NULL,
    stream_id TEXT NOT NULL,
    fingerprint TEXT NOT NULL CHECK (length(fingerprint) = 64),
    hub_id TEXT NOT NULL DEFAULT '',
    last_created_seq INTEGER NOT NULL DEFAULT 0 CHECK (typeof(last_created_seq) = 'integer' AND last_created_seq >= 0),
    committed_seq INTEGER NOT NULL DEFAULT 0 CHECK (typeof(committed_seq) = 'integer' AND committed_seq >= 0 AND committed_seq <= last_created_seq),
    connection_generation INTEGER NOT NULL DEFAULT 0 CHECK (typeof(connection_generation) = 'integer' AND connection_generation >= 0),
    config_revision INTEGER NOT NULL DEFAULT 0 CHECK (typeof(config_revision) = 'integer' AND config_revision >= 0)
);

CREATE TABLE IF NOT EXISTS edge_credentials (
    kind TEXT PRIMARY KEY CHECK (kind IN ('enrollment', 'runtime')),
    token_hash BLOB NOT NULL CHECK (length(token_hash) = 32),
    hub_id TEXT NOT NULL DEFAULT '',
    enrollment_id TEXT NOT NULL DEFAULT '',
    version INTEGER NOT NULL DEFAULT 0 CHECK (typeof(version) = 'integer' AND version >= 0),
    issued_at INTEGER NOT NULL,
    expires_at INTEGER,
    CHECK ((kind = 'enrollment' AND version = 0 AND hub_id = '' AND enrollment_id = '' AND expires_at > issued_at) OR
           (kind = 'runtime' AND version > 0 AND hub_id <> '' AND enrollment_id <> '' AND expires_at IS NULL))
);

CREATE TABLE IF NOT EXISTS edge_config (
    revision INTEGER PRIMARY KEY CHECK (typeof(revision) = 'integer' AND revision >= 1),
    hub_id TEXT NOT NULL,
    probe_id TEXT NOT NULL,
    schema_version INTEGER NOT NULL CHECK (schema_version = 1),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64),
    created_at INTEGER NOT NULL,
    effective_at INTEGER NOT NULL,
    applied_at INTEGER NOT NULL,
    key_confirmation TEXT NOT NULL CHECK (length(key_confirmation) = 64),
    protected_payload BLOB NOT NULL CHECK (length(protected_payload) BETWEEN 30 AND 16777245)
);

CREATE TABLE IF NOT EXISTS edge_assignments (
    monitor_id INTEGER PRIMARY KEY CHECK (monitor_id > 0),
    generation INTEGER NOT NULL CHECK (typeof(generation) = 'integer' AND generation > 0),
    active INTEGER NOT NULL CHECK (active IN (0, 1)),
    revision INTEGER NOT NULL REFERENCES edge_config(revision)
);
