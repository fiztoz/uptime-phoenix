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
-- SQLite cannot do microsecond arithmetic on its stored text stamps, so the exact
-- fixed lifetime is not a CHECK here: the store's whole-receipt validation
-- (domain.ProbeCleanup.Valid()) refuses any row whose deadline differs, and
-- MariaDB additionally pins it by CHECK.
CREATE TABLE IF NOT EXISTS probe_install_cleanups (
    cleanup_id TEXT NOT NULL PRIMARY KEY CHECK (length(cleanup_id) = 36 AND substr(cleanup_id, 9, 1) = '-' AND substr(cleanup_id, 14, 1) = '-' AND substr(cleanup_id, 19, 1) = '-' AND substr(cleanup_id, 24, 1) = '-' AND length(replace(cleanup_id, '-', '')) = 32 AND replace(cleanup_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND cleanup_id <> '00000000-0000-0000-0000-000000000000'),
    operation_id TEXT NOT NULL CHECK (length(operation_id) = 36 AND substr(operation_id, 9, 1) = '-' AND substr(operation_id, 14, 1) = '-' AND substr(operation_id, 19, 1) = '-' AND substr(operation_id, 24, 1) = '-' AND length(replace(operation_id, '-', '')) = 32 AND replace(operation_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND operation_id <> '00000000-0000-0000-0000-000000000000'),
    client_key_fingerprint TEXT NOT NULL CHECK (client_key_fingerprint LIKE 'SHA256:%'),
    requested_by INTEGER NOT NULL CHECK (typeof(requested_by) = 'integer' AND requested_by > 0),
    owner_id TEXT NOT NULL CHECK (length(owner_id) = 36 AND substr(owner_id, 9, 1) = '-' AND substr(owner_id, 14, 1) = '-' AND substr(owner_id, 19, 1) = '-' AND substr(owner_id, 24, 1) = '-' AND length(replace(owner_id, '-', '')) = 32 AND replace(owner_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND owner_id <> '00000000-0000-0000-0000-000000000000'),
    state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'cleaned', 'failed', 'interrupted')),
    error_code TEXT NULL CHECK (error_code IS NULL OR error_code IN ('not_started', 'owner_interrupted', 'deadline_expired', 'outcome_unknown')),
    attempt_id TEXT NULL CHECK (attempt_id IS NULL OR (length(attempt_id) = 36 AND substr(attempt_id, 9, 1) = '-' AND substr(attempt_id, 14, 1) = '-' AND substr(attempt_id, 19, 1) = '-' AND substr(attempt_id, 24, 1) = '-' AND length(replace(attempt_id, '-', '')) = 32 AND replace(attempt_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND attempt_id <> '00000000-0000-0000-0000-000000000000')),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    deadline_at TIMESTAMP NOT NULL,
    CHECK (deadline_at > created_at),
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
    UNIQUE (attempt_id),
    UNIQUE (cleanup_id, attempt_id, owner_id),
    FOREIGN KEY (operation_id) REFERENCES probe_install_operations (operation_id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_probe_install_cleanups_operation ON probe_install_cleanups (operation_id, created_at, cleanup_id);

-- M8 irreversible one-shot host-action admission for the cleanup's single
-- attempt. It has the shape of probe_host_action_admissions (086) but is keyed by
-- the cleanup attempt and references the cleanup row that reserved it (cleanup,
-- attempt and owner must all match), so no other admission store can consume a
-- cleanup attempt and vice versa. No signature, private key, token, raw output,
-- applied state, renewal or rearm is stored here.
CREATE TABLE IF NOT EXISTS probe_cleanup_admissions (
    attempt_id TEXT NOT NULL PRIMARY KEY CHECK (length(attempt_id) = 36 AND substr(attempt_id, 9, 1) = '-' AND substr(attempt_id, 14, 1) = '-' AND substr(attempt_id, 19, 1) = '-' AND substr(attempt_id, 24, 1) = '-' AND length(replace(attempt_id, '-', '')) = 32 AND replace(attempt_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND attempt_id <> '00000000-0000-0000-0000-000000000000'),
    cleanup_id TEXT NOT NULL CHECK (length(cleanup_id) = 36 AND substr(cleanup_id, 9, 1) = '-' AND substr(cleanup_id, 14, 1) = '-' AND substr(cleanup_id, 19, 1) = '-' AND substr(cleanup_id, 24, 1) = '-' AND length(replace(cleanup_id, '-', '')) = 32 AND replace(cleanup_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND cleanup_id <> '00000000-0000-0000-0000-000000000000'),
    owner_id TEXT NOT NULL CHECK (length(owner_id) = 36 AND substr(owner_id, 9, 1) = '-' AND substr(owner_id, 14, 1) = '-' AND substr(owner_id, 19, 1) = '-' AND substr(owner_id, 24, 1) = '-' AND length(replace(owner_id, '-', '')) = 32 AND replace(owner_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND owner_id <> '00000000-0000-0000-0000-000000000000'),
    authority_key_id TEXT NOT NULL CHECK (length(authority_key_id) = 64 AND authority_key_id NOT GLOB '*[^0-9a-f]*'),
    nonce_hex TEXT NOT NULL CHECK (length(nonce_hex) = 64 AND nonce_hex NOT GLOB '*[^0-9a-f]*' AND nonce_hex <> '0000000000000000000000000000000000000000000000000000000000000000'),
    marker_sha256 TEXT NOT NULL CHECK (length(marker_sha256) = 64 AND marker_sha256 NOT GLOB '*[^0-9a-f]*'),
    admitted_at TIMESTAMP NOT NULL,
    UNIQUE (cleanup_id),
    UNIQUE (authority_key_id, nonce_hex),
    FOREIGN KEY (cleanup_id, attempt_id, owner_id) REFERENCES probe_install_cleanups (cleanup_id, attempt_id, owner_id) ON DELETE RESTRICT
);
