ALTER TABLE edge_identity ADD COLUMN initial_stream_id TEXT NOT NULL DEFAULT '';
UPDATE edge_identity SET initial_stream_id = stream_id;

-- Reservation is durable before archiving. Normal runtime must refuse to open a
-- pending operation, so a restart cannot invalidate an already published copy.
CREATE TABLE edge_stream_resets (
    reset_id TEXT PRIMARY KEY,
    previous_stream_id TEXT NOT NULL UNIQUE,
    stream_id TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('prepared', 'applied')),
    record BLOB NOT NULL CHECK (length(record) BETWEEN 1 AND 8192),
    proof BLOB NOT NULL CHECK (length(proof) = 29)
);
CREATE UNIQUE INDEX idx_edge_reset_pending ON edge_stream_resets (state) WHERE state = 'prepared';
