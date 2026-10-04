-- A failed guard is intentional: overall projections would be discarded.

CREATE TABLE IF NOT EXISTS probe_health_projection_down_guard (
    state_count INTEGER NOT NULL CHECK (state_count = 0)
);
DELETE FROM probe_health_projection_down_guard;
INSERT INTO probe_health_projection_down_guard (state_count)
SELECT (SELECT COUNT(*) FROM monitor_health_state)
     + (SELECT COUNT(*) FROM monitor_health_history);
DROP INDEX IF EXISTS idx_probe_dirty_resolution;
DROP TABLE monitor_health_history;
DROP TABLE monitor_health_state;
DROP TABLE probe_health_projection_down_guard;
