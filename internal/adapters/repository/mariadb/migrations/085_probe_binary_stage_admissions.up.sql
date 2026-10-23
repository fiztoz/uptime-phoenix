-- M8 irreversible one-shot stage admission. No signature, private key, token,
-- raw output, applied state, renewal or rearm is stored here.
CREATE TABLE IF NOT EXISTS probe_binary_stage_admissions (
    operation_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(operation_id) = 36 AND SUBSTRING(operation_id, 9, 1) = '-' AND SUBSTRING(operation_id, 14, 1) = '-' AND SUBSTRING(operation_id, 19, 1) = '-' AND SUBSTRING(operation_id, 24, 1) = '-' AND CAST(REPLACE(operation_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND operation_id <> '00000000-0000-0000-0000-000000000000'),
    owner_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(owner_id) = 36 AND SUBSTRING(owner_id, 9, 1) = '-' AND SUBSTRING(owner_id, 14, 1) = '-' AND SUBSTRING(owner_id, 19, 1) = '-' AND SUBSTRING(owner_id, 24, 1) = '-' AND CAST(REPLACE(owner_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND owner_id <> '00000000-0000-0000-0000-000000000000'),
    attempt_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(attempt_id) = 36 AND SUBSTRING(attempt_id, 9, 1) = '-' AND SUBSTRING(attempt_id, 14, 1) = '-' AND SUBSTRING(attempt_id, 19, 1) = '-' AND SUBSTRING(attempt_id, 24, 1) = '-' AND CAST(REPLACE(attempt_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND attempt_id <> '00000000-0000-0000-0000-000000000000'),
    kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (kind = 'artifact_directory'),
    authority_key_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(authority_key_id) = 64 AND CAST(authority_key_id AS BINARY) REGEXP '^[0-9a-f]{64}$'),
    nonce_hex VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(nonce_hex) = 64 AND CAST(nonce_hex AS BINARY) REGEXP '^[0-9a-f]{64}$' AND nonce_hex <> '0000000000000000000000000000000000000000000000000000000000000000'),
    marker_sha256 VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(marker_sha256) = 64 AND CAST(marker_sha256 AS BINARY) REGEXP '^[0-9a-f]{64}$'),
    admitted_at DATETIME(6) NOT NULL,
    PRIMARY KEY (operation_id, kind),
    UNIQUE KEY uq_probe_stage_admission_attempt (attempt_id),
    UNIQUE KEY uq_probe_stage_admission_nonce (authority_key_id, nonce_hex),
    CONSTRAINT fk_probe_stage_admission_resource FOREIGN KEY (operation_id, kind) REFERENCES probe_install_resources (operation_id, kind) ON DELETE RESTRICT
) ENGINE=InnoDB;
