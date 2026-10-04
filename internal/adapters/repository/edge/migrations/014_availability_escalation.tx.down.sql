-- Populated ladder progress cannot be reconstructed after the columns are gone.
CREATE TABLE edge_escalation_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO edge_escalation_downgrade_guard SELECT 0 FROM edge_alerts WHERE escalation_status IS NOT NULL LIMIT 1;
INSERT INTO edge_escalation_downgrade_guard SELECT 0 FROM edge_delivery_outbox WHERE escalation_policy_id > 0 OR escalation_step > 0 LIMIT 1;
DROP TABLE edge_escalation_downgrade_guard;

DROP TRIGGER IF EXISTS edge_alerts_escalation_insert;
DROP TRIGGER IF EXISTS edge_alerts_escalation_update;
DROP INDEX IF EXISTS idx_edge_alert_escalation_due;

ALTER TABLE edge_delivery_outbox DROP COLUMN escalation_step;
ALTER TABLE edge_delivery_outbox DROP COLUMN escalation_policy_id;
ALTER TABLE edge_alerts DROP COLUMN escalation_next_run_at;
ALTER TABLE edge_alerts DROP COLUMN escalation_next_step;
ALTER TABLE edge_alerts DROP COLUMN escalation_status;
ALTER TABLE edge_alerts DROP COLUMN escalation_policy_version;
ALTER TABLE edge_alerts DROP COLUMN escalation_policy_id;
