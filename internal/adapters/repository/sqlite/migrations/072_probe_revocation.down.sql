-- Refuse to erase a durable revocation or make a deleted identity active again.
CREATE TEMP TABLE guard_probe_revocation (n INTEGER CHECK (n = 0));
INSERT INTO guard_probe_revocation SELECT COUNT(*) FROM probe_revocations;
INSERT INTO guard_probe_revocation SELECT COUNT(*) FROM probes WHERE revoked_at IS NOT NULL OR deleted_at IS NOT NULL;
DROP TABLE guard_probe_revocation;
DROP TABLE probe_revocations;
ALTER TABLE probes DROP COLUMN deleted_at;
ALTER TABLE probes DROP COLUMN revoked_at;
