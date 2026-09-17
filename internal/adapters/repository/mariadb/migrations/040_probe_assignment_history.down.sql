-- Stop all writers first: MariaDB DDL auto-commits.
-- Only untouched local revision-one history is safely reproducible.
CREATE TABLE IF NOT EXISTS probe_assignment_history_down_guard (
    state_count BIGINT NOT NULL CHECK (state_count = 0)
) ENGINE=InnoDB;
DELETE FROM probe_assignment_history_down_guard;
INSERT INTO probe_assignment_history_down_guard (state_count)
SELECT COUNT(*) FROM monitor_probe_assignment_history
WHERE probe_id <> 'local' OR generation <> 1 OR revision <> 1
   OR health_policy <> 'any_down' OR ended_at IS NOT NULL;
DROP TABLE monitor_probe_assignment_history;
DROP TABLE probe_assignment_history_down_guard;
