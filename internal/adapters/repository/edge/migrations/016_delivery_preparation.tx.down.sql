-- Refuse loss of durable preparation tuples or receipts on downgrade.
CREATE TEMP TABLE edge_delivery_preparation_downgrade_guard (safe INTEGER CHECK (safe = 1));
INSERT INTO edge_delivery_preparation_downgrade_guard SELECT 0 FROM edge_delivery_preparations LIMIT 1;
INSERT INTO edge_delivery_preparation_downgrade_guard SELECT 0 FROM edge_applied_commands WHERE kind = 'delivery.prepare' LIMIT 1;
DROP TABLE edge_delivery_preparation_downgrade_guard;
DROP TABLE edge_delivery_preparations;
CREATE TABLE edge_applied_commands_before_prepare (
 command_id TEXT PRIMARY KEY,
 kind TEXT NOT NULL,
 request_hash BLOB NOT NULL CHECK (length(request_hash) = 32),
 status TEXT NOT NULL CHECK (status IN ('applied', 'already_applied', 'already_resolved', 'rejected', 'expired')),
 applied_at INTEGER,
 code TEXT NOT NULL,
 message TEXT NOT NULL,
 retain_until INTEGER NOT NULL,
 credential_version INTEGER CHECK (credential_version IS NULL OR (typeof(credential_version) = 'integer' AND credential_version > 0 AND kind = 'credential.prepare' AND status IN ('applied', 'already_applied'))),
 certificate_version INTEGER CHECK (certificate_version IS NULL OR (typeof(certificate_version) = 'integer' AND certificate_version > 1 AND kind = 'certificate.prepare' AND status IN ('applied', 'already_applied'))),
 certificate_fingerprint TEXT CHECK (certificate_fingerprint IS NULL OR (length(certificate_fingerprint) = 64 AND certificate_version IS NOT NULL)),
 certificate_not_after INTEGER CHECK (certificate_not_after IS NULL OR (typeof(certificate_not_after) = 'integer' AND certificate_version IS NOT NULL)),
 CHECK ((status IN ('applied', 'already_applied', 'already_resolved') AND applied_at IS NOT NULL AND code = '') OR
        (status = 'rejected' AND applied_at IS NULL AND code <> '') OR
        (status = 'expired' AND applied_at IS NULL))
);
INSERT INTO edge_applied_commands_before_prepare SELECT * FROM edge_applied_commands;
DROP TABLE edge_applied_commands;
ALTER TABLE edge_applied_commands_before_prepare RENAME TO edge_applied_commands;
CREATE INDEX idx_edge_command_retention ON edge_applied_commands (retain_until, command_id);
