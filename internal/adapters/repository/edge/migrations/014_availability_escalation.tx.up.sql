-- Source-owned availability escalation progress. The hub already stores these
-- fields on probe_incidents; the edge is the writer. Other subjects stay empty.
-- SQLite accepts new nullable columns without rebuilding the alert table.
ALTER TABLE edge_alerts ADD COLUMN escalation_policy_id INTEGER;
ALTER TABLE edge_alerts ADD COLUMN escalation_policy_version INTEGER;
ALTER TABLE edge_alerts ADD COLUMN escalation_status TEXT;
ALTER TABLE edge_alerts ADD COLUMN escalation_next_step INTEGER;
ALTER TABLE edge_alerts ADD COLUMN escalation_next_run_at INTEGER;

CREATE INDEX idx_edge_alert_escalation_due ON edge_alerts (escalation_next_run_at, source_alert_id)
    WHERE escalation_status = 'pending';

-- Step identity on a delivery lets the sender revalidate the exact rung. Step
-- zero stays the direct notification and keeps both columns at zero.
ALTER TABLE edge_delivery_outbox ADD COLUMN escalation_policy_id INTEGER NOT NULL DEFAULT 0 CHECK (escalation_policy_id >= 0);
ALTER TABLE edge_delivery_outbox ADD COLUMN escalation_step INTEGER NOT NULL DEFAULT 0 CHECK (escalation_step >= 0);

-- NEW. is required. A trigger that names a column added by ALTER TABLE without
-- the NEW qualifier is planned against the pre-alter table and raises
-- "no such column" on every later insert.
CREATE TRIGGER edge_alerts_escalation_insert BEFORE INSERT ON edge_alerts
WHEN NOT (
    (COALESCE(NEW.escalation_policy_id, 0) = 0 AND COALESCE(NEW.escalation_policy_version, 0) = 0 AND COALESCE(NEW.escalation_status, '') = '' AND NEW.escalation_next_step IS NULL AND NEW.escalation_next_run_at IS NULL)
    OR (NEW.subject_kind = 'availability' AND NEW.status = 'firing' AND NEW.escalation_policy_id > 0 AND NEW.escalation_policy_version > 0 AND NEW.escalation_policy_version <= NEW.config_revision AND NEW.escalation_status = 'pending' AND NEW.escalation_next_step > 0 AND NEW.escalation_next_run_at IS NOT NULL)
    OR (NEW.subject_kind = 'availability' AND NEW.escalation_policy_id > 0 AND NEW.escalation_policy_version > 0 AND NEW.escalation_policy_version <= NEW.config_revision AND NEW.escalation_status IN ('done', 'canceled') AND NEW.escalation_next_step IS NULL AND NEW.escalation_next_run_at IS NULL)
)
BEGIN
    SELECT RAISE(ABORT, 'edge escalation invariant');
END;

CREATE TRIGGER edge_alerts_escalation_update BEFORE UPDATE ON edge_alerts
WHEN NOT (
    (COALESCE(NEW.escalation_policy_id, 0) = 0 AND COALESCE(NEW.escalation_policy_version, 0) = 0 AND COALESCE(NEW.escalation_status, '') = '' AND NEW.escalation_next_step IS NULL AND NEW.escalation_next_run_at IS NULL)
    OR (NEW.subject_kind = 'availability' AND NEW.status = 'firing' AND NEW.escalation_policy_id > 0 AND NEW.escalation_policy_version > 0 AND NEW.escalation_policy_version <= NEW.config_revision AND NEW.escalation_status = 'pending' AND NEW.escalation_next_step > 0 AND NEW.escalation_next_run_at IS NOT NULL)
    OR (NEW.subject_kind = 'availability' AND NEW.escalation_policy_id > 0 AND NEW.escalation_policy_version > 0 AND NEW.escalation_policy_version <= NEW.config_revision AND NEW.escalation_status IN ('done', 'canceled') AND NEW.escalation_next_step IS NULL AND NEW.escalation_next_run_at IS NULL)
)
BEGIN
    SELECT RAISE(ABORT, 'edge escalation invariant');
END;
