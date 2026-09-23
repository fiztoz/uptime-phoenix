-- Refuse to drop the columns while any capacity evidence or mirrored promotion
-- exists. A downgrade must never silently discard source capacity history.
CREATE TEMPORARY TABLE probe_capacity_state_downgrade_guard (n INTEGER CHECK (n = 0));
INSERT INTO probe_capacity_state_downgrade_guard SELECT COUNT(*) FROM probe_observations WHERE conditions_json IS NOT NULL;
INSERT INTO probe_capacity_state_downgrade_guard SELECT COUNT(*) FROM monitor_probe_state WHERE conditions_json IS NOT NULL;
INSERT INTO probe_capacity_state_downgrade_guard SELECT COUNT(*) FROM monitor_conditions WHERE source_seq > 0;
DROP TABLE probe_capacity_state_downgrade_guard;
ALTER TABLE monitor_conditions DROP COLUMN source_seq;
ALTER TABLE monitor_probe_state DROP COLUMN conditions_json;
ALTER TABLE probe_observations DROP COLUMN conditions_json;
