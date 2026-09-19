-- Stop writers. An initialized installation record must not be silently discarded.
CREATE TABLE IF NOT EXISTS probe_installation_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO probe_installation_downgrade_guard (ok) SELECT 0 FROM probe_installation LIMIT 1;
DROP TABLE probe_installation_downgrade_guard;
DROP TABLE probe_installation;
