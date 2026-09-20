ALTER TABLE probe_connections ADD COLUMN credential_high_water INTEGER NOT NULL DEFAULT 0 CHECK (typeof(credential_high_water) = 'integer' AND credential_high_water >= 0);
UPDATE probe_connections SET credential_high_water = credential_version;
ALTER TABLE probe_commands ADD COLUMN result_credential_version INTEGER NULL
 CHECK (result_credential_version IS NULL OR (typeof(result_credential_version) = 'integer' AND result_credential_version > 0 AND kind = 'credential.prepare' AND status IN ('applied', 'already_applied')));
CREATE TABLE probe_credential_rotations (
 rotation_id TEXT PRIMARY KEY,
 hub_id TEXT NOT NULL,
 probe_id TEXT NOT NULL REFERENCES probes(id),
 stream_id TEXT NOT NULL,
 enrollment_id TEXT NOT NULL,
 credential_version INTEGER NOT NULL,
 previous_version INTEGER NOT NULL,
 endpoint TEXT NOT NULL,
 fingerprint TEXT NOT NULL,
 protected_credential BLOB NOT NULL,
 prepare_command_id TEXT NOT NULL UNIQUE,
 activate_command_id TEXT NOT NULL UNIQUE,
 created_at TEXT NOT NULL,
 overlap_expires_at TEXT NOT NULL,
 state TEXT NOT NULL,
 prepared_at TEXT NULL,
 activated_at TEXT NULL,
 failure_code TEXT NOT NULL DEFAULT '',
 updated_at TEXT NOT NULL,
 retain_until TEXT NOT NULL,
 UNIQUE (probe_id, credential_version),
 CHECK (typeof(credential_version) = 'integer' AND typeof(previous_version) = 'integer' AND credential_version > previous_version AND previous_version > 0),
 CHECK (overlap_expires_at > created_at),
 CHECK (length(protected_credential) BETWEEN 30 AND 300),
 CHECK (state IN ('preparing', 'activating', 'active', 'failed')),
 CHECK (state <> 'active' OR (prepared_at IS NOT NULL AND activated_at IS NOT NULL))
);
CREATE INDEX idx_probe_credential_pending ON probe_credential_rotations (probe_id, state, credential_version);
