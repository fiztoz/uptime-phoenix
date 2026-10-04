-- A released epoch must also survive: deleting it would admit stale owners.
CREATE TABLE IF NOT EXISTS probe_runtime_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO probe_runtime_downgrade_guard SELECT 0 FROM probe_runtime_owners LIMIT 1;
DROP TABLE probe_runtime_downgrade_guard;
DROP TABLE probe_runtime_owners;
