-- Run only with all application processes stopped. A failed guard is intentional:
-- remote registrations/tombstones or changed assignment policy/revision require
-- an explicit backup/export and compatible rollback, never silent deletion.
CREATE TABLE IF NOT EXISTS probe_registry_down_guard (
    state_count INTEGER NOT NULL CHECK (state_count = 0)
);
DELETE FROM probe_registry_down_guard;
INSERT INTO probe_registry_down_guard (state_count)
SELECT (SELECT COUNT(*) FROM probes WHERE id <> 'local')
     + (SELECT COUNT(*) FROM monitor_probe_assignments WHERE probe_id <> 'local' OR generation <> 1 OR active <> 1)
     + (SELECT COUNT(*) FROM monitor_probe_assignment_sets WHERE revision <> 1 OR health_policy <> 'any_down');
DROP TABLE monitor_probe_assignments;
DROP TABLE monitor_probe_assignment_sets;
DROP TABLE probes;
DROP TABLE probe_registry_down_guard;
