ALTER TABLE probe_connections ADD COLUMN credential_high_water BIGINT NOT NULL DEFAULT 0 CHECK (credential_high_water >= 0);
UPDATE probe_connections SET credential_high_water = credential_version;
ALTER TABLE probe_commands ADD COLUMN result_credential_version BIGINT NULL
 CHECK (result_credential_version IS NULL OR (result_credential_version > 0 AND kind = 'credential.prepare' AND status IN ('applied', 'already_applied')));
CREATE TABLE probe_credential_rotations (
 rotation_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
 hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 enrollment_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 credential_version BIGINT NOT NULL,
 previous_version BIGINT NOT NULL,
 endpoint VARCHAR(2048) NOT NULL,
 fingerprint VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 protected_credential BLOB NOT NULL,
 prepare_command_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
 activate_command_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
 created_at DATETIME(6) NOT NULL,
 overlap_expires_at DATETIME(6) NOT NULL,
 state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 prepared_at DATETIME(6) NULL,
 activated_at DATETIME(6) NULL,
 failure_code VARCHAR(128) NOT NULL DEFAULT '',
 updated_at DATETIME(6) NOT NULL,
 retain_until DATETIME(6) NOT NULL,
 UNIQUE KEY uq_probe_credential_version (probe_id, credential_version),
 KEY idx_probe_credential_pending (probe_id, state, credential_version),
 CONSTRAINT fk_probe_credential_rotation_probe FOREIGN KEY (probe_id) REFERENCES probes(id),
 CHECK (credential_version > previous_version AND previous_version > 0),
 CHECK (overlap_expires_at > created_at),
 CHECK (OCTET_LENGTH(protected_credential) BETWEEN 30 AND 300),
 CHECK (state IN ('preparing', 'activating', 'active', 'failed')),
 CHECK (state <> 'active' OR (prepared_at IS NOT NULL AND activated_at IS NOT NULL))
);
