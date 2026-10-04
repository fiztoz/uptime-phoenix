-- Stop all application writers before downgrade. Refuse to discard regional state.
CREATE TABLE IF NOT EXISTS probe_auxiliary_down_guard (
    state_count INTEGER NOT NULL CHECK (state_count = 0)
);
DELETE FROM probe_auxiliary_down_guard;
INSERT INTO probe_auxiliary_down_guard (state_count)
SELECT (SELECT COUNT(*) FROM monitor_conditions WHERE probe_id <> 'local' OR assignment_generation <> 1)
     + (SELECT COUNT(*) FROM tls_info WHERE probe_id <> 'local' OR assignment_generation <> 1);
ALTER TABLE monitor_conditions
    DROP PRIMARY KEY,
    ADD PRIMARY KEY (monitor_id, kind),
    DROP COLUMN probe_id,
    DROP COLUMN assignment_generation;
ALTER TABLE tls_info ADD UNIQUE KEY monitor_id (monitor_id);
ALTER TABLE tls_info
    DROP INDEX uq_tls_info_assignment,
    DROP COLUMN probe_id,
    DROP COLUMN assignment_generation;
DROP TABLE probe_auxiliary_down_guard;
