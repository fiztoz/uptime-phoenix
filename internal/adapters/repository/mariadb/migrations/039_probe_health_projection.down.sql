-- Run only with all application processes stopped; MariaDB DDL auto-commits.
-- A failed guard is intentional: overall projections would be discarded.

CREATE TABLE IF NOT EXISTS probe_health_projection_down_guard (
    state_count BIGINT NOT NULL CHECK (state_count = 0)
) ENGINE=InnoDB;
DELETE FROM probe_health_projection_down_guard;
INSERT INTO probe_health_projection_down_guard (state_count)
SELECT (SELECT COUNT(*) FROM monitor_health_state)
     + (SELECT COUNT(*) FROM monitor_health_history);
DROP INDEX idx_probe_dirty_resolution ON probe_dirty_buckets;
DROP TABLE monitor_health_history;
DROP TABLE monitor_health_state;
DROP TABLE probe_health_projection_down_guard;
