CREATE TABLE probe_stream_resets (
 reset_id TEXT PRIMARY KEY,
 hub_id TEXT NOT NULL,
 probe_id TEXT NOT NULL REFERENCES probes(id) ON DELETE RESTRICT,
 previous_stream_id TEXT NOT NULL UNIQUE,
 stream_id TEXT NOT NULL UNIQUE,
 enrollment_id TEXT NOT NULL,
 fingerprint TEXT NOT NULL CHECK (length(fingerprint) = 64),
 credential_version INTEGER NOT NULL CHECK (typeof(credential_version) = 'integer' AND credential_version > 0),
 certificate_version INTEGER NOT NULL CHECK (typeof(certificate_version) = 'integer' AND certificate_version > 0),
 hub_committed_seq INTEGER NOT NULL CHECK (typeof(hub_committed_seq) = 'integer' AND hub_committed_seq >= 0),
 connection_generation INTEGER NOT NULL CHECK (typeof(connection_generation) = 'integer' AND connection_generation > 0),
 prepared_at TIMESTAMP NOT NULL,
 state TEXT NOT NULL CHECK (state IN ('prepared', 'awaiting_peer', 'complete')),
 source_receipt BLOB,
 activated_at TIMESTAMP,
 confirmed_at TIMESTAMP,
 CHECK (previous_stream_id <> stream_id),
 CHECK ((state = 'prepared' AND source_receipt IS NULL AND activated_at IS NULL AND confirmed_at IS NULL) OR
        (state = 'awaiting_peer' AND source_receipt IS NOT NULL AND length(source_receipt) BETWEEN 1 AND 8192 AND activated_at IS NOT NULL AND confirmed_at IS NULL) OR
        (state = 'complete' AND source_receipt IS NOT NULL AND length(source_receipt) BETWEEN 1 AND 8192 AND activated_at IS NOT NULL AND confirmed_at IS NOT NULL))
);
CREATE INDEX idx_probe_reset_phase ON probe_stream_resets (probe_id, state);
ALTER TABLE probe_missing_state ADD COLUMN reason TEXT NOT NULL DEFAULT 'missing_snapshot_state' CHECK (reason IN ('missing_snapshot_state', 'stream_reset'));
