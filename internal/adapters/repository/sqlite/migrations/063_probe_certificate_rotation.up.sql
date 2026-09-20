ALTER TABLE probe_connections ADD COLUMN certificate_version INTEGER NOT NULL DEFAULT 1 CHECK (certificate_version >= 1);
ALTER TABLE probe_connections ADD COLUMN certificate_high_water INTEGER NOT NULL DEFAULT 1 CHECK (certificate_high_water >= certificate_version);
ALTER TABLE probe_connections ADD COLUMN certificate_not_after TEXT NULL;
ALTER TABLE probe_commands ADD COLUMN result_certificate_version INTEGER NULL CHECK (result_certificate_version IS NULL OR (result_certificate_version > 1 AND kind = 'certificate.prepare' AND status IN ('applied', 'already_applied')));
ALTER TABLE probe_commands ADD COLUMN result_certificate_fingerprint TEXT NULL;
ALTER TABLE probe_commands ADD COLUMN result_certificate_not_after TEXT NULL;
CREATE TABLE probe_certificate_rotations (
 rotation_id TEXT PRIMARY KEY,
 hub_id TEXT NOT NULL,
 probe_id TEXT NOT NULL REFERENCES probes(id),
 stream_id TEXT NOT NULL,
 enrollment_id TEXT NOT NULL,
 credential_version INTEGER NOT NULL CHECK (credential_version > 0),
 endpoint TEXT NOT NULL,
 previous_fingerprint TEXT NOT NULL,
 certificate_version INTEGER NOT NULL,
 previous_version INTEGER NOT NULL,
 valid_for_days INTEGER NOT NULL CHECK (valid_for_days BETWEEN 1 AND 3650),
 prepare_command_id TEXT NOT NULL UNIQUE,
 activate_command_id TEXT NOT NULL UNIQUE,
 created_at TEXT NOT NULL,
 overlap_expires_at TEXT NOT NULL,
 overlap_closed INTEGER NOT NULL DEFAULT FALSE,
 state TEXT NOT NULL,
 prepared_at TEXT NULL,
 activated_at TEXT NULL,
 certificate_not_after TEXT NULL,
 fingerprint TEXT NULL,
 failure_code TEXT NOT NULL DEFAULT '',
 updated_at TEXT NOT NULL,
 retain_until TEXT NOT NULL,
 UNIQUE (probe_id, certificate_version),
 CHECK (certificate_version > previous_version AND previous_version >= 1),
 CHECK (overlap_expires_at > created_at),
 CHECK (state IN ('preparing', 'activating', 'active', 'failed')),
 CHECK (state NOT IN ('activating', 'active') OR (prepared_at IS NOT NULL AND fingerprint IS NOT NULL AND certificate_not_after IS NOT NULL)),
 CHECK (state <> 'active' OR activated_at IS NOT NULL)
);
CREATE INDEX idx_probe_certificate_pending ON probe_certificate_rotations (probe_id, state, certificate_version);
