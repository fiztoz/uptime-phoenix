-- Never discard candidate identities, rotation receipts or unpromoted high-water.
CREATE TABLE IF NOT EXISTS probe_rotation_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO probe_rotation_downgrade_guard SELECT 0 FROM probe_credential_rotations LIMIT 1;
INSERT INTO probe_rotation_downgrade_guard SELECT 0 FROM probe_commands WHERE kind IN ('credential.prepare', 'credential.activate') LIMIT 1;
INSERT INTO probe_rotation_downgrade_guard SELECT 0 FROM probe_connections WHERE credential_high_water > credential_version LIMIT 1;
DROP TABLE probe_rotation_downgrade_guard;
DROP TABLE probe_credential_rotations;
ALTER TABLE probe_commands DROP COLUMN result_credential_version;
ALTER TABLE probe_connections DROP COLUMN credential_high_water;
