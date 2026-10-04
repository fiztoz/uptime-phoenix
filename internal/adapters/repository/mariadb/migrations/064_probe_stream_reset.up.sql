CREATE TABLE probe_stream_resets (
 reset_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
 hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL REFERENCES probes(id),
 previous_stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
 stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
 enrollment_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 fingerprint VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (length(fingerprint) = 64),
 credential_version BIGINT NOT NULL CHECK (credential_version > 0),
 certificate_version BIGINT NOT NULL CHECK (certificate_version > 0),
 hub_committed_seq BIGINT NOT NULL CHECK (hub_committed_seq >= 0),
 connection_generation BIGINT NOT NULL CHECK (connection_generation > 0),
 prepared_at DATETIME(6) NOT NULL,
 state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (state IN ('prepared', 'awaiting_peer', 'complete')),
 source_receipt BLOB NULL,
 activated_at DATETIME(6) NULL,
 confirmed_at DATETIME(6) NULL,
 CHECK (previous_stream_id <> stream_id),
 CHECK ((state = 'prepared' AND source_receipt IS NULL AND activated_at IS NULL AND confirmed_at IS NULL) OR
        (state = 'awaiting_peer' AND source_receipt IS NOT NULL AND OCTET_LENGTH(source_receipt) BETWEEN 1 AND 8192 AND activated_at IS NOT NULL AND confirmed_at IS NULL) OR
        (state = 'complete' AND source_receipt IS NOT NULL AND OCTET_LENGTH(source_receipt) BETWEEN 1 AND 8192 AND activated_at IS NOT NULL AND confirmed_at IS NOT NULL))
);
CREATE INDEX idx_probe_reset_phase ON probe_stream_resets (probe_id, state);
ALTER TABLE probe_missing_state ADD COLUMN reason VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'missing_snapshot_state' CHECK (reason IN ('missing_snapshot_state', 'stream_reset'));
