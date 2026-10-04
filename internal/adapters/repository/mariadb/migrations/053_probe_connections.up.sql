CREATE TABLE IF NOT EXISTS probe_connections (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
    enrollment_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
    credential_version BIGINT NOT NULL CHECK (credential_version > 0),
    endpoint VARCHAR(2048) NOT NULL,
    fingerprint VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (LENGTH(fingerprint) = 64),
    protected_credential VARBINARY(300) NOT NULL CHECK (LENGTH(protected_credential) BETWEEN 30 AND 300),
    state VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (state IN ('prepared', 'active')),
    prepared_at DATETIME(6) NOT NULL,
    activated_at DATETIME(6),
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT,
    CHECK ((state = 'prepared' AND activated_at IS NULL) OR (state = 'active' AND activated_at IS NOT NULL))
) ENGINE=InnoDB;
