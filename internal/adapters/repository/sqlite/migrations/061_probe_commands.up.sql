-- Legacy metadata-only rows remain readable; only fully protected rows dispatch.
ALTER TABLE probe_commands ADD COLUMN hub_id TEXT NULL;
ALTER TABLE probe_commands ADD COLUMN stream_id TEXT NULL;
ALTER TABLE probe_commands ADD COLUMN payload_sha256 TEXT NULL;
ALTER TABLE probe_commands ADD COLUMN protected_payload BLOB NULL;
ALTER TABLE probe_commands ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE probe_commands ADD COLUMN last_attempt_at TEXT NULL;
ALTER TABLE probe_commands ADD COLUMN next_attempt_at TEXT NULL;
ALTER TABLE probe_commands ADD COLUMN result_applied_at TEXT NULL;
ALTER TABLE probe_commands ADD COLUMN result_code TEXT NOT NULL DEFAULT '';
ALTER TABLE probe_commands ADD COLUMN result_message TEXT NOT NULL DEFAULT '';
ALTER TABLE probe_commands ADD COLUMN retain_until TEXT NULL;
CREATE INDEX idx_probe_commands_due ON probe_commands (probe_id, remote_confirmed, next_attempt_at, command_id);
CREATE INDEX idx_probe_commands_retention ON probe_commands (probe_id, retain_until);
