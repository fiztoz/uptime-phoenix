-- Prepared snapshots only. No applied/active state and no plaintext configuration.
CREATE TABLE probe_config_snapshots (
    probe_id TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK (typeof(revision) = 'integer' AND revision > 0),
    hub_id TEXT NOT NULL CHECK (length(hub_id) = 36),
    schema_version INTEGER NOT NULL CHECK (schema_version = 1),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    source_created_at TIMESTAMP NOT NULL,
    effective_at TIMESTAMP NOT NULL,
    protected_payload BLOB NOT NULL CHECK (typeof(protected_payload) = 'blob' AND length(protected_payload) BETWEEN 30 AND 16777245 AND substr(protected_payload, 1, 1) = X'01'),
    stored_at TIMESTAMP NOT NULL,
    PRIMARY KEY (probe_id, revision),
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);
