ALTER TABLE probe_connections ADD COLUMN certificate_version BIGINT NOT NULL DEFAULT 1 CHECK (certificate_version >= 1);
ALTER TABLE probe_connections ADD COLUMN certificate_high_water BIGINT NOT NULL DEFAULT 1 CHECK (certificate_high_water >= certificate_version);
ALTER TABLE probe_connections ADD COLUMN certificate_not_after DATETIME(6) NULL;
ALTER TABLE probe_commands ADD COLUMN result_certificate_version BIGINT NULL CHECK (result_certificate_version IS NULL OR (result_certificate_version > 1 AND kind = 'certificate.prepare' AND status IN ('applied', 'already_applied')));
ALTER TABLE probe_commands ADD COLUMN result_certificate_fingerprint VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL;
ALTER TABLE probe_commands ADD COLUMN result_certificate_not_after DATETIME(6) NULL;
CREATE TABLE probe_certificate_rotations (
 rotation_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
 hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL REFERENCES probes(id),
 stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 enrollment_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 credential_version BIGINT NOT NULL CHECK (credential_version > 0),
 endpoint VARCHAR(2048) NOT NULL,
 previous_fingerprint VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 certificate_version BIGINT NOT NULL,
 previous_version BIGINT NOT NULL,
 valid_for_days INTEGER NOT NULL CHECK (valid_for_days BETWEEN 1 AND 3650),
 prepare_command_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
 activate_command_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
 created_at DATETIME(6) NOT NULL,
 overlap_expires_at DATETIME(6) NOT NULL,
 overlap_closed BOOLEAN NOT NULL DEFAULT FALSE,
 state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 prepared_at DATETIME(6) NULL,
 activated_at DATETIME(6) NULL,
 certificate_not_after DATETIME(6) NULL,
 fingerprint VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
 failure_code VARCHAR(128) NOT NULL DEFAULT '',
 updated_at DATETIME(6) NOT NULL,
 retain_until DATETIME(6) NOT NULL,
 UNIQUE (probe_id, certificate_version),
 CHECK (certificate_version > previous_version AND previous_version >= 1),
 CHECK (overlap_expires_at > created_at),
 CHECK (state IN ('preparing', 'activating', 'active', 'failed')),
 CHECK (state NOT IN ('activating', 'active') OR (prepared_at IS NOT NULL AND fingerprint IS NOT NULL AND certificate_not_after IS NOT NULL)),
 CHECK (state <> 'active' OR activated_at IS NOT NULL)
);
CREATE INDEX idx_probe_certificate_pending ON probe_certificate_rotations (probe_id, state, certificate_version);
