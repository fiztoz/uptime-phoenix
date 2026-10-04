CREATE TABLE IF NOT EXISTS probe_session_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO probe_session_downgrade_guard SELECT 0 FROM probe_sessions LIMIT 1;
DROP TABLE probe_session_downgrade_guard;
DROP TABLE probe_sessions;
