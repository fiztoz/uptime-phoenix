CREATE TABLE probe_reset_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO probe_reset_downgrade_guard SELECT 0 FROM probe_stream_resets LIMIT 1;
INSERT INTO probe_reset_downgrade_guard SELECT 0 FROM probe_missing_state WHERE reason <> 'missing_snapshot_state' LIMIT 1;
DROP TABLE probe_reset_downgrade_guard;
ALTER TABLE probe_missing_state DROP COLUMN reason;
DROP TABLE probe_stream_resets;
