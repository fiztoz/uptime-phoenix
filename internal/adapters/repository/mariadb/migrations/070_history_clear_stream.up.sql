-- Sequence numbers belong to a stream, not an assignment generation.
-- Backfill the stream that existed when the legacy clear committed, never a
-- replacement created after that clear. Unenrolled probes retain a time fence.
ALTER TABLE history_clear_watermarks ADD COLUMN IF NOT EXISTS through_stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '';
UPDATE history_clear_watermarks
SET through_stream_id = COALESCE((
    SELECT s.stream_id FROM probe_streams s
    WHERE s.probe_id = history_clear_watermarks.probe_id
      AND s.created_at <= history_clear_watermarks.cleared_at
    ORDER BY s.created_at DESC, s.stream_id DESC LIMIT 1
), '')
WHERE through_stream_id = '';
