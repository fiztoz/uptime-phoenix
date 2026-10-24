-- M8 durable intentional install cleanup: one operator-initiated removal of the
-- host resources one root systemd install operation created (unit, artifact
-- directory, data directory). A row holds only the nonsecret cleanup request
-- (its own ID, the install operation it targets, the cleanup's transient
-- deployment key PUBLIC fingerprint and the requesting actor), the immutable
-- owner UUID, fixed state/error codes and DB-clock stamps. The install identity
-- itself is never copied: it is read from probe_install_operations, whose row is
-- the shared authority lock and whose FK below restricts its deletion. The
-- deployment private key, key-file paths, passwords and raw remote output have no
-- column by construction.
-- The owner UUID fences every write in the database. The 5-minute deadline is a
-- fixed DB-clock bound with no renewal, takeover, resume or rearm. A cleanup is a
-- single step: attempt_id is NULL until the queued-to-running transition reserves
-- it, and it is unique across all cleanups.
-- The state/error/attempt CHECKs below encode domain.ProbeCleanup.Valid() exactly
-- (including liveness: only deadline_expired may carry updated_at at or after the
-- deadline).
-- The exact fixed lifetime is pinned here by CHECK on MariaDB and, on both engines,
-- by the store's whole-receipt validation (domain.ProbeCleanup.Valid()).
CREATE TABLE IF NOT EXISTS probe_install_cleanups (
    cleanup_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY CHECK (CHAR_LENGTH(cleanup_id) = 36 AND SUBSTRING(cleanup_id, 9, 1) = '-' AND SUBSTRING(cleanup_id, 14, 1) = '-' AND SUBSTRING(cleanup_id, 19, 1) = '-' AND SUBSTRING(cleanup_id, 24, 1) = '-' AND CAST(REPLACE(cleanup_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND cleanup_id <> '00000000-0000-0000-0000-000000000000'),
    operation_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(operation_id) = 36 AND SUBSTRING(operation_id, 9, 1) = '-' AND SUBSTRING(operation_id, 14, 1) = '-' AND SUBSTRING(operation_id, 19, 1) = '-' AND SUBSTRING(operation_id, 24, 1) = '-' AND CAST(REPLACE(operation_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND operation_id <> '00000000-0000-0000-0000-000000000000'),
    client_key_fingerprint VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (client_key_fingerprint LIKE 'SHA256:%'),
    requested_by BIGINT NOT NULL CHECK (requested_by > 0),
    owner_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(owner_id) = 36 AND SUBSTRING(owner_id, 9, 1) = '-' AND SUBSTRING(owner_id, 14, 1) = '-' AND SUBSTRING(owner_id, 19, 1) = '-' AND SUBSTRING(owner_id, 24, 1) = '-' AND CAST(REPLACE(owner_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND owner_id <> '00000000-0000-0000-0000-000000000000'),
    state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (state IN ('queued', 'running', 'cleaned', 'failed', 'interrupted')),
    error_code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL CHECK (error_code IS NULL OR error_code IN ('not_started', 'owner_interrupted', 'deadline_expired', 'outcome_unknown')),
    attempt_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL CHECK (attempt_id IS NULL OR (CHAR_LENGTH(attempt_id) = 36 AND SUBSTRING(attempt_id, 9, 1) = '-' AND SUBSTRING(attempt_id, 14, 1) = '-' AND SUBSTRING(attempt_id, 19, 1) = '-' AND SUBSTRING(attempt_id, 24, 1) = '-' AND CAST(REPLACE(attempt_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND attempt_id <> '00000000-0000-0000-0000-000000000000')),
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    deadline_at DATETIME(6) NOT NULL,
    CHECK (deadline_at > created_at),
    CHECK (deadline_at = created_at + INTERVAL 300 SECOND),
    CHECK (updated_at >= created_at),
    CHECK (
        (state = 'queued' AND error_code IS NULL AND attempt_id IS NULL AND updated_at < deadline_at) OR
        (state IN ('running', 'cleaned') AND error_code IS NULL AND attempt_id IS NOT NULL AND updated_at < deadline_at) OR
        (state = 'failed' AND error_code IS NOT NULL AND error_code = 'not_started' AND attempt_id IS NOT NULL AND updated_at < deadline_at) OR
        (state = 'interrupted' AND error_code IS NOT NULL AND (
            (error_code = 'owner_interrupted' AND attempt_id IS NULL AND updated_at < deadline_at) OR
            (error_code = 'deadline_expired' AND updated_at >= deadline_at) OR
            (error_code = 'outcome_unknown' AND attempt_id IS NOT NULL AND updated_at < deadline_at)))
    ),
    KEY idx_probe_install_cleanups_operation (operation_id, created_at, cleanup_id),
    UNIQUE KEY uq_probe_install_cleanups_attempt (attempt_id),
    UNIQUE KEY uq_probe_install_cleanups_binding (cleanup_id, attempt_id, owner_id),
    CONSTRAINT fk_probe_install_cleanup_operation FOREIGN KEY (operation_id) REFERENCES probe_install_operations (operation_id) ON DELETE RESTRICT
) ENGINE=InnoDB;

-- M8 irreversible one-shot host-action admission for the cleanup's single
-- attempt. It has the shape of probe_host_action_admissions (086) but is keyed by
-- the cleanup attempt and references the cleanup row that reserved it (cleanup,
-- attempt and owner must all match), so no other admission store can consume a
-- cleanup attempt and vice versa. No signature, private key, token, raw output,
-- applied state, renewal or rearm is stored here.
CREATE TABLE IF NOT EXISTS probe_cleanup_admissions (
    attempt_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY CHECK (CHAR_LENGTH(attempt_id) = 36 AND SUBSTRING(attempt_id, 9, 1) = '-' AND SUBSTRING(attempt_id, 14, 1) = '-' AND SUBSTRING(attempt_id, 19, 1) = '-' AND SUBSTRING(attempt_id, 24, 1) = '-' AND CAST(REPLACE(attempt_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND attempt_id <> '00000000-0000-0000-0000-000000000000'),
    cleanup_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(cleanup_id) = 36 AND SUBSTRING(cleanup_id, 9, 1) = '-' AND SUBSTRING(cleanup_id, 14, 1) = '-' AND SUBSTRING(cleanup_id, 19, 1) = '-' AND SUBSTRING(cleanup_id, 24, 1) = '-' AND CAST(REPLACE(cleanup_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND cleanup_id <> '00000000-0000-0000-0000-000000000000'),
    owner_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(owner_id) = 36 AND SUBSTRING(owner_id, 9, 1) = '-' AND SUBSTRING(owner_id, 14, 1) = '-' AND SUBSTRING(owner_id, 19, 1) = '-' AND SUBSTRING(owner_id, 24, 1) = '-' AND CAST(REPLACE(owner_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND owner_id <> '00000000-0000-0000-0000-000000000000'),
    authority_key_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(authority_key_id) = 64 AND CAST(authority_key_id AS BINARY) REGEXP '^[0-9a-f]{64}$'),
    nonce_hex VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(nonce_hex) = 64 AND CAST(nonce_hex AS BINARY) REGEXP '^[0-9a-f]{64}$' AND nonce_hex <> '0000000000000000000000000000000000000000000000000000000000000000'),
    marker_sha256 VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(marker_sha256) = 64 AND CAST(marker_sha256 AS BINARY) REGEXP '^[0-9a-f]{64}$'),
    admitted_at DATETIME(6) NOT NULL,
    UNIQUE KEY uq_probe_cleanup_admission_cleanup (cleanup_id),
    UNIQUE KEY uq_probe_cleanup_admission_nonce (authority_key_id, nonce_hex),
    CONSTRAINT fk_probe_cleanup_admission_cleanup FOREIGN KEY (cleanup_id, attempt_id, owner_id) REFERENCES probe_install_cleanups (cleanup_id, attempt_id, owner_id) ON DELETE RESTRICT
) ENGINE=InnoDB;
