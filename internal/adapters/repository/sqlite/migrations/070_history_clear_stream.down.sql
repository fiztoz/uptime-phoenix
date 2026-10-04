-- Removing stream scope with retained fences would suppress a replacement
-- stream's fresh observations. Refuse rather than silently change authority.
CREATE TEMPORARY TABLE history_clear_stream_downgrade_guard (n INTEGER CHECK (n = 0));
INSERT INTO history_clear_stream_downgrade_guard SELECT COUNT(*) FROM history_clear_watermarks;
DROP TABLE history_clear_stream_downgrade_guard;
ALTER TABLE history_clear_watermarks DROP COLUMN through_stream_id;
