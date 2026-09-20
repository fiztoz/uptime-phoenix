-- Stop writers; never discard protected requests or retained results.
CREATE TABLE IF NOT EXISTS probe_commands_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO probe_commands_downgrade_guard SELECT 0 FROM probe_commands WHERE hub_id IS NOT NULL LIMIT 1;
DROP TABLE probe_commands_downgrade_guard;
CREATE INDEX IF NOT EXISTS idx_probe_commands_probe_id ON probe_commands (probe_id);
DROP INDEX IF EXISTS idx_probe_commands_due ON probe_commands;
DROP INDEX IF EXISTS idx_probe_commands_retention ON probe_commands;
ALTER TABLE probe_commands DROP COLUMN retain_until;
ALTER TABLE probe_commands DROP COLUMN result_message;
ALTER TABLE probe_commands DROP COLUMN result_code;
ALTER TABLE probe_commands DROP COLUMN result_applied_at;
ALTER TABLE probe_commands DROP COLUMN next_attempt_at;
ALTER TABLE probe_commands DROP COLUMN last_attempt_at;
ALTER TABLE probe_commands DROP COLUMN attempts;
ALTER TABLE probe_commands DROP COLUMN protected_payload;
ALTER TABLE probe_commands DROP COLUMN payload_sha256;
ALTER TABLE probe_commands DROP COLUMN stream_id;
ALTER TABLE probe_commands DROP COLUMN hub_id;
