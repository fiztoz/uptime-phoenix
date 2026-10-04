-- Do not discard live pins, pending operations, receipts or version high-water.
CREATE TABLE IF NOT EXISTS probe_certificate_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO probe_certificate_downgrade_guard SELECT 0 FROM probe_certificate_rotations LIMIT 1;
INSERT INTO probe_certificate_downgrade_guard SELECT 0 FROM probe_commands WHERE kind IN ('certificate.prepare', 'certificate.activate') LIMIT 1;
INSERT INTO probe_certificate_downgrade_guard SELECT 0 FROM probe_connections WHERE certificate_version <> 1 OR certificate_high_water <> 1 OR certificate_not_after IS NOT NULL LIMIT 1;
DROP TABLE probe_certificate_downgrade_guard;
DROP TABLE probe_certificate_rotations;
ALTER TABLE probe_commands DROP COLUMN result_certificate_not_after;
ALTER TABLE probe_commands DROP COLUMN result_certificate_fingerprint;
ALTER TABLE probe_commands DROP COLUMN result_certificate_version;
ALTER TABLE probe_connections DROP COLUMN certificate_not_after;
ALTER TABLE probe_connections DROP COLUMN certificate_high_water;
ALTER TABLE probe_connections DROP COLUMN certificate_version;
