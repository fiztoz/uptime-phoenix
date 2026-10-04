-- Stop all writers. Unpublished mappings may be regenerated after downgrade.
CREATE TABLE IF NOT EXISTS alert_source_downgrade_guard (ok INT NOT NULL CHECK (ok = 1));
INSERT INTO alert_source_downgrade_guard (ok)
SELECT 0 FROM alerts JOIN probe_incidents ON probe_incidents.source_alert_id = alerts.source_alert_id LIMIT 1;
DROP TABLE alert_source_downgrade_guard;
ALTER TABLE alerts
    DROP INDEX uq_alerts_source_identity,
    DROP CONSTRAINT ck_alerts_source_identity,
    DROP COLUMN source_alert_id,
    DROP COLUMN transition_version;
