-- Prepared snapshots only. No applied/active state and no plaintext configuration.
CREATE TABLE probe_config_snapshots (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(hub_id) = 36),
    schema_version INT NOT NULL CHECK (schema_version = 1),
    sha256 VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (sha256 REGEXP '^[0-9a-f]{64}$'),
    source_created_at DATETIME(6) NOT NULL,
    effective_at DATETIME(6) NOT NULL,
    protected_payload LONGBLOB NOT NULL CHECK (OCTET_LENGTH(protected_payload) BETWEEN 30 AND 16777245 AND SUBSTRING(protected_payload, 1, 1) = X'01'),
    stored_at DATETIME(6) NOT NULL,
    PRIMARY KEY (probe_id, revision),
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;
