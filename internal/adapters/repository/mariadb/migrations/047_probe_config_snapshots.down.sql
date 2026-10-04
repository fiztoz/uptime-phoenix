-- Stop writers. Retained prepared revisions must not be silently discarded.
CREATE TABLE IF NOT EXISTS probe_config_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO probe_config_downgrade_guard (ok) SELECT 0 FROM probe_config_snapshots LIMIT 1;
DROP TABLE probe_config_downgrade_guard;
DROP TABLE probe_config_snapshots;
