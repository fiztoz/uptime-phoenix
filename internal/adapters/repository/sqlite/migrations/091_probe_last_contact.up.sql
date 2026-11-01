-- Hub-observed last contact for remote probes (issue #133). See the MariaDB
-- migration of the same number for the full rationale.
ALTER TABLE probe_sessions ADD COLUMN last_contact_at INTEGER NULL CHECK (last_contact_at IS NULL OR last_contact_at > 0);
