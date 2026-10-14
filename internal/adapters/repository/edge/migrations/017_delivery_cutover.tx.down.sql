-- Downgrade refuses to erase live source gates, unresolved provider ambiguity or
-- durable cutover receipts: the older schema can represent none of them, and
-- dropping an ambiguous attempt would fabricate a cleared I/O outcome.
CREATE TEMP TABLE edge_delivery_cutover_downgrade_guard (safe INTEGER CHECK (safe = 1));
INSERT INTO edge_delivery_cutover_downgrade_guard SELECT 0 FROM edge_delivery_cutover_gates WHERE state <> 'released' LIMIT 1;
INSERT INTO edge_delivery_cutover_downgrade_guard SELECT 0 FROM edge_provider_attempts WHERE state IN ('in_flight', 'ambiguous') LIMIT 1;
INSERT INTO edge_delivery_cutover_downgrade_guard SELECT 0 FROM edge_applied_commands WHERE kind IN ('delivery.quiesce', 'delivery.apply', 'delivery.resume') LIMIT 1;
DROP TABLE edge_delivery_cutover_downgrade_guard;
DROP TABLE edge_provider_attempts;
DROP TABLE edge_delivery_cutover_gates;
-- Restore the preparation-era receipt ledger while preserving every populated
-- preparation row and receipt column exactly.
CREATE TEMP TABLE edge_delivery_preparations_hold AS SELECT * FROM edge_delivery_preparations;
DROP TABLE edge_delivery_preparations;
CREATE TABLE edge_applied_commands_pre_cutover (
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
 CHECK ((kind <> 'delivery.prepare' AND status IN ('applied', 'already_applied', 'already_resolved') AND applied_at IS NOT NULL AND code = '') OR
        (kind = 'delivery.prepare' AND status IN ('applied', 'already_applied') AND applied_at IS NOT NULL AND code = 'delivery_prepared') OR
        (status = 'rejected' AND applied_at IS NULL AND code <> '') OR
        (status = 'expired' AND applied_at IS NULL))
);
INSERT INTO edge_applied_commands_pre_cutover SELECT * FROM edge_applied_commands;
DROP TABLE edge_applied_commands;
ALTER TABLE edge_applied_commands_pre_cutover RENAME TO edge_applied_commands;
CREATE INDEX idx_edge_command_retention ON edge_applied_commands (retain_until, command_id);
CREATE TABLE edge_delivery_preparations (
 command_id TEXT PRIMARY KEY REFERENCES edge_applied_commands(command_id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
 hub_id TEXT NOT NULL,
 probe_id TEXT NOT NULL,
 stream_id TEXT NOT NULL,
 monitor_id INTEGER NOT NULL CHECK (monitor_id > 0),
 assignment_revision INTEGER NOT NULL CHECK (assignment_revision > 0),
 generation INTEGER NOT NULL CHECK (generation > 0),
 config_revision INTEGER NOT NULL REFERENCES edge_config(revision) ON DELETE RESTRICT CHECK (config_revision > 0),
 sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
 alert_delivery TEXT NOT NULL CHECK (alert_delivery IN ('aggregate', 'both')),
 prepared_at INTEGER NOT NULL CHECK (prepared_at > 0)
);
INSERT INTO edge_delivery_preparations SELECT * FROM edge_delivery_preparations_hold;
DROP TABLE edge_delivery_preparations_hold;
