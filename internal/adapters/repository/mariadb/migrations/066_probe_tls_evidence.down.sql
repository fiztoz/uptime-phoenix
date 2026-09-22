CREATE TEMPORARY TABLE probe_tls_downgrade_guard (n INTEGER CHECK (n = 0));
INSERT INTO probe_tls_downgrade_guard SELECT COUNT(*) FROM probe_observations WHERE tls_json IS NOT NULL;
INSERT INTO probe_tls_downgrade_guard SELECT COUNT(*) FROM monitor_probe_state WHERE tls_json IS NOT NULL;
DROP TABLE probe_tls_downgrade_guard;
ALTER TABLE monitor_probe_state DROP COLUMN tls_json;
ALTER TABLE probe_observations DROP COLUMN tls_json;
