-- Run only with all application processes stopped; MariaDB DDL auto-commits.
-- A failed guard is intentional: remote registrations/tombstones or changed
-- policy/revision require explicit backup/export and compatible rollback.
CREATE TABLE IF NOT EXISTS probe_registry_down_guard (
    state_count BIGINT NOT NULL CHECK (state_count = 0)
) ENGINE=InnoDB;
DELETE FROM probe_registry_down_guard;
INSERT INTO probe_registry_down_guard (state_count)
SELECT (SELECT COUNT(*) FROM probes WHERE id <> 'local')
     + (SELECT COUNT(*) FROM monitor_probe_assignments WHERE probe_id <> 'local' OR generation <> 1 OR active <> 1)
     + (SELECT COUNT(*) FROM monitor_probe_assignment_sets WHERE revision <> 1 OR health_policy <> 'any_down');
DROP TABLE monitor_probe_assignments;
DROP TABLE monitor_probe_assignment_sets;
DROP TABLE probes;
DROP TABLE probe_registry_down_guard;
