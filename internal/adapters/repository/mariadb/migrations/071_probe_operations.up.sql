-- Durable administrative operations and the frozen registration network trust.
-- Operations are persisted before any 202 is returned: an operation row is the
-- only proof that accepted work exists. Endpoint/pin live on the registration
-- (identity, immutable after create); enrollment copies them into the prepared
-- connection. Neither is authentication material.
CREATE TABLE IF NOT EXISTS probe_operations (
    operation_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY CHECK (CHAR_LENGTH(operation_id) = 36),
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (kind IN ('enroll', 'rotate_credential', 'reset_stream')),
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed')),
    phase VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (phase REGEXP '^[a-z0-9_]{1,128}$'),
    error_code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    error_message VARCHAR(255) NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    CHECK ((status = 'failed' AND error_code IS NOT NULL AND error_message IS NOT NULL) OR
           (status <> 'failed' AND error_code IS NULL AND error_message IS NULL)),
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

ALTER TABLE probes ADD COLUMN IF NOT EXISTS endpoint VARCHAR(2048) NULL;
ALTER TABLE probes ADD COLUMN IF NOT EXISTS tls_fingerprint VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL;
