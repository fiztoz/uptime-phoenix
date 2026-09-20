-- Retained key material and version high-water cannot be discarded by downgrade.
CREATE TABLE edge_certificate_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO edge_certificate_downgrade_guard SELECT 0 FROM edge_certificate_rotations LIMIT 1;
INSERT INTO edge_certificate_downgrade_guard SELECT 0 FROM edge_certificate_state WHERE highest_version > 1;
INSERT INTO edge_certificate_downgrade_guard SELECT 0 FROM edge_applied_commands WHERE kind IN ('certificate.prepare', 'certificate.activate') LIMIT 1;
DROP TABLE edge_certificate_downgrade_guard;
DROP TABLE edge_certificate_rotations;
DROP TABLE edge_certificate_state;
ALTER TABLE edge_applied_commands DROP COLUMN certificate_not_after;
ALTER TABLE edge_applied_commands DROP COLUMN certificate_fingerprint;
ALTER TABLE edge_applied_commands DROP COLUMN certificate_version;
