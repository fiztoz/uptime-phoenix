-- Stop all writers. Refuse to discard remote or later-generation incident identity.
CREATE TABLE IF NOT EXISTS probe_alert_scope_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO probe_alert_scope_downgrade_guard (ok)
SELECT 0 FROM alerts WHERE probe_id <> 'local' OR assignment_generation <> 1 LIMIT 1;
DROP TABLE probe_alert_scope_downgrade_guard;
ALTER TABLE alerts
    ADD UNIQUE KEY uq_alerts_open_monitor (open_monitor_id),
    DROP INDEX uq_alerts_open_assignment,
    DROP COLUMN probe_id,
    DROP COLUMN assignment_generation;
