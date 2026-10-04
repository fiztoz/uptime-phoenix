-- Legacy metadata-only rows remain readable; only fully protected rows dispatch.
ALTER TABLE probe_commands ADD COLUMN hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL;
ALTER TABLE probe_commands ADD COLUMN stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL;
ALTER TABLE probe_commands ADD COLUMN payload_sha256 VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL;
ALTER TABLE probe_commands ADD COLUMN protected_payload MEDIUMBLOB NULL;
ALTER TABLE probe_commands ADD COLUMN attempts BIGINT NOT NULL DEFAULT 0;
ALTER TABLE probe_commands ADD COLUMN last_attempt_at DATETIME(6) NULL;
ALTER TABLE probe_commands ADD COLUMN next_attempt_at DATETIME(6) NULL;
ALTER TABLE probe_commands ADD COLUMN result_applied_at DATETIME(6) NULL;
ALTER TABLE probe_commands ADD COLUMN result_code VARCHAR(128) NOT NULL DEFAULT '';
ALTER TABLE probe_commands ADD COLUMN result_message VARCHAR(4096) NOT NULL DEFAULT '';
ALTER TABLE probe_commands ADD COLUMN retain_until DATETIME(6) NULL;
-- Preserve an explicit base index; InnoDB may replace an implicit FK index
-- when the new covering indexes appear. Downgrade still needs probe_id indexed.
CREATE INDEX IF NOT EXISTS idx_probe_commands_probe_id ON probe_commands (probe_id);
CREATE INDEX idx_probe_commands_due ON probe_commands (probe_id, remote_confirmed, next_attempt_at, command_id);
CREATE INDEX idx_probe_commands_retention ON probe_commands (probe_id, retain_until);
