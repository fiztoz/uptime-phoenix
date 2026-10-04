CREATE TABLE IF NOT EXISTS probe_regional_down_guard (
    state_count INTEGER NOT NULL CHECK (state_count = 0)
);
DELETE FROM probe_regional_down_guard;
INSERT INTO probe_regional_down_guard (state_count)
SELECT (SELECT COUNT(*) FROM probe_observations)
     + (SELECT COUNT(*) FROM monitor_probe_state)
     + (SELECT COUNT(*) FROM probe_streams WHERE committed_seq <> 0)
     + (SELECT COUNT(*) FROM probe_commands)
     + (SELECT COUNT(*) FROM probe_dirty_buckets);
DROP TABLE probe_dirty_buckets;
DROP TABLE probe_commands;
DROP TABLE monitor_probe_state;
DROP TABLE probe_observations;
DROP TABLE probe_streams;
DROP TABLE probe_regional_down_guard;
