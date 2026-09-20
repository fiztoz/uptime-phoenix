-- Refuse to lose pending identities, version high-water or retained receipts.
CREATE TABLE edge_rotation_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO edge_rotation_downgrade_guard SELECT 0 FROM edge_credential_rotations LIMIT 1;
INSERT INTO edge_rotation_downgrade_guard SELECT 0 FROM edge_applied_commands WHERE kind IN ('credential.prepare', 'credential.activate') LIMIT 1;
INSERT INTO edge_rotation_downgrade_guard SELECT 0 FROM edge_credential_state
 WHERE highest_version > COALESCE((SELECT version FROM edge_credentials WHERE kind = 'runtime'), 0);
DROP TABLE edge_rotation_downgrade_guard;
DROP TABLE edge_credential_rotations;
DROP TABLE edge_credential_state;
ALTER TABLE edge_applied_commands DROP COLUMN credential_version;
