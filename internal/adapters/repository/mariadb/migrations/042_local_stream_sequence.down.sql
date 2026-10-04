-- Refuse to lose the sequence high-water mark, even after history deletion.
-- Stop all writers before downgrade; MariaDB DDL auto-commits.
CREATE TABLE IF NOT EXISTS probe_local_sequence_down_guard (
    state_count INTEGER NOT NULL CHECK (state_count = 0)
);
DELETE FROM probe_local_sequence_down_guard;
INSERT INTO probe_local_sequence_down_guard (state_count)
SELECT COUNT(*) FROM probe_local_sequence WHERE last_seq <> 0;
DROP TABLE probe_local_sequence;
DROP TABLE probe_local_sequence_down_guard;
