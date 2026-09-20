-- Dropping epoch provenance would permit old sequence numbers to be reused.
CREATE TABLE edge_reset_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO edge_reset_downgrade_guard SELECT 0 FROM edge_stream_resets LIMIT 1;
INSERT INTO edge_reset_downgrade_guard SELECT 0 FROM edge_identity WHERE initial_stream_id <> stream_id;
DROP TABLE edge_reset_downgrade_guard;
DROP TABLE edge_stream_resets;
ALTER TABLE edge_identity DROP COLUMN initial_stream_id;
