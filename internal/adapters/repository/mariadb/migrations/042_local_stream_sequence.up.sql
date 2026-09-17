-- Stop all writers for upgrade. The counter survives monitor/history deletion.
CREATE TABLE IF NOT EXISTS probe_local_sequence (
    id INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    last_seq BIGINT NOT NULL CHECK (last_seq >= 0)
) ENGINE=InnoDB;

-- Preserve the greatest retained local-stream sequence, including heartbeats
-- saved before a failed regional commit in the older recording path.
INSERT INTO probe_local_sequence (id, last_seq)
SELECT 1, COALESCE(MAX(last_seq), 0) FROM (
    SELECT MAX(seq) AS last_seq FROM probe_observations WHERE stream_id = '00000000-0000-4000-8000-000000000001'
    UNION ALL
    SELECT MAX(seq) AS last_seq FROM monitor_probe_state WHERE stream_id = '00000000-0000-4000-8000-000000000001'
    UNION ALL
    SELECT MAX(source_seq) AS last_seq FROM heartbeats WHERE stream_id = '00000000-0000-4000-8000-000000000001'
) AS retained_sequences WHERE 1 = 1
ON DUPLICATE KEY UPDATE last_seq = GREATEST(probe_local_sequence.last_seq, VALUES(last_seq));
