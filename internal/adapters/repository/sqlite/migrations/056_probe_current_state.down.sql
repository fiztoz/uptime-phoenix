CREATE TEMPORARY TABLE probe_state_downgrade_guard (n INTEGER CHECK (n = 0));
INSERT INTO probe_state_downgrade_guard SELECT COUNT(*) FROM probe_state_receipts;
INSERT INTO probe_state_downgrade_guard SELECT COUNT(*) FROM probe_missing_state;
DROP TABLE probe_state_downgrade_guard;
DROP TABLE probe_missing_state;
DROP TABLE probe_state_receipts;
ALTER TABLE monitor_probe_state DROP COLUMN active_source_alert_id;
ALTER TABLE monitor_probe_state DROP COLUMN message;
ALTER TABLE monitor_probe_state DROP COLUMN ping;
