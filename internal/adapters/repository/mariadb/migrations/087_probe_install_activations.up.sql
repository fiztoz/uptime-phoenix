-- M8 durable probe activation (init, register, start, enroll) for one
-- resources_ready systemd install. Rows hold only the frozen nonsecret
-- activation request, PUBLIC identity facts reported by probe init (probe id,
-- stream id, certificate SHA-256 pin), actor/owner IDs, fixed state/error codes
-- and the ordered step receipts. The one-time enrollment token, private keys,
-- key-file paths, passwords and raw remote output are never stored: no column
-- for them exists by construction.
-- The owner UUID fences every write in the database; the 15-minute deadline is
-- a fixed DB-clock bound with no renewal, takeover, resume or epoch reuse. The
-- install journal (084-086) is untouched; its operation row is the shared
-- authority lock and the FK below restricts its deletion.
CREATE TABLE IF NOT EXISTS probe_install_activations (
    operation_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY CHECK (CHAR_LENGTH(operation_id) = 36),
    probe_key VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(probe_key) BETWEEN 1 AND 63 AND probe_key <> 'local'),
    name VARCHAR(200) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (CHAR_LENGTH(name) BETWEEN 1 AND 200),
    location VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (CHAR_LENGTH(location) BETWEEN 0 AND 255),
    listen_port INT NOT NULL CHECK (listen_port BETWEEN 1024 AND 65535),
    client_key_fingerprint VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (client_key_fingerprint LIKE 'SHA256:%'),
    requested_by BIGINT NOT NULL CHECK (requested_by > 0),
    owner_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(owner_id) = 36 AND SUBSTRING(owner_id, 9, 1) = '-' AND SUBSTRING(owner_id, 14, 1) = '-' AND SUBSTRING(owner_id, 19, 1) = '-' AND SUBSTRING(owner_id, 24, 1) = '-' AND CAST(REPLACE(owner_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND owner_id <> '00000000-0000-0000-0000-000000000000'),
    state VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (state IN ('queued', 'running', 'active', 'cancel_requested', 'failed', 'canceled', 'interrupted')),
    error_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL CHECK (error_code IS NULL OR error_code IN ('unsupported_host', 'artifact_rejected', 'resource_conflict', 'execution_failed', 'permission_denied', 'runtime_unverified', 'not_started', 'internal', 'canceled', 'owner_interrupted', 'deadline_expired', 'outcome_unknown')),
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    certificate_fingerprint VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    deadline_at DATETIME(6) NOT NULL,
    CHECK (deadline_at > created_at),
    CHECK (updated_at >= created_at),
    CHECK (
        (probe_id IS NULL AND stream_id IS NULL AND certificate_fingerprint IS NULL) OR
        (probe_id IS NOT NULL AND stream_id IS NOT NULL AND certificate_fingerprint IS NOT NULL
            AND CHAR_LENGTH(probe_id) = 36 AND SUBSTRING(probe_id, 9, 1) = '-' AND SUBSTRING(probe_id, 14, 1) = '-' AND SUBSTRING(probe_id, 19, 1) = '-' AND SUBSTRING(probe_id, 24, 1) = '-' AND CAST(REPLACE(probe_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND probe_id <> '00000000-0000-0000-0000-000000000000'
            AND CHAR_LENGTH(stream_id) = 36 AND SUBSTRING(stream_id, 9, 1) = '-' AND SUBSTRING(stream_id, 14, 1) = '-' AND SUBSTRING(stream_id, 19, 1) = '-' AND SUBSTRING(stream_id, 24, 1) = '-' AND CAST(REPLACE(stream_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND stream_id <> '00000000-0000-0000-0000-000000000000'
            AND CHAR_LENGTH(certificate_fingerprint) = 64 AND CAST(certificate_fingerprint AS BINARY) REGEXP '^[0-9a-f]{64}$')
    ),
    CHECK (state <> 'queued' OR probe_id IS NULL),
    CHECK (
        (state IN ('queued', 'running', 'active', 'cancel_requested') AND error_code IS NULL) OR
        (state = 'failed' AND error_code IS NOT NULL AND error_code IN ('unsupported_host', 'artifact_rejected', 'resource_conflict', 'execution_failed', 'permission_denied', 'runtime_unverified', 'not_started', 'internal')) OR
        (state = 'canceled' AND error_code IS NOT NULL AND error_code = 'canceled') OR
        (state = 'interrupted' AND error_code IS NOT NULL AND error_code IN ('owner_interrupted', 'deadline_expired', 'outcome_unknown'))
    ),
    CONSTRAINT fk_probe_install_activation_operation FOREIGN KEY (operation_id) REFERENCES probe_install_operations (operation_id) ON DELETE RESTRICT
) ENGINE=InnoDB;

-- One immutable attempt per canonical step (init, registration, start,
-- enrollment). attempt_id is a unique attempt UUID; planned rows carry no
-- attempt residue at all. The FK restricts parent deletion.
CREATE TABLE IF NOT EXISTS probe_install_activation_steps (
    operation_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (kind IN ('probe_init', 'registration', 'service_start', 'enrollment')),
    ordinal INT NOT NULL CHECK (ordinal BETWEEN 1 AND 4),
    reference VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(reference) BETWEEN 1 AND 512),
    owned TINYINT(1) NOT NULL CHECK (owned IN (0, 1)),
    state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (state IN ('planned', 'in_flight', 'applied', 'not_started', 'unknown')),
    attempt_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL CHECK (attempt_id IS NULL OR CHAR_LENGTH(attempt_id) = 36),
    error_code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL CHECK (error_code IS NULL OR error_code IN ('not_started', 'outcome_unknown', 'deadline_expired', 'owner_interrupted')),
    started_at DATETIME(6) NULL,
    finished_at DATETIME(6) NULL,
    PRIMARY KEY (operation_id, kind),
    UNIQUE KEY uq_probe_install_activation_steps_ordinal (operation_id, ordinal),
    UNIQUE KEY uq_probe_install_activation_steps_attempt (attempt_id),
    CONSTRAINT fk_probe_install_activation_steps_activation FOREIGN KEY (operation_id) REFERENCES probe_install_activations (operation_id) ON DELETE RESTRICT,
    CHECK (
        (kind = 'probe_init' AND ordinal = 1 AND owned = 1) OR
        (kind = 'registration' AND ordinal = 2 AND owned = 0) OR
        (kind = 'service_start' AND ordinal = 3 AND owned = 1) OR
        (kind = 'enrollment' AND ordinal = 4 AND owned = 0)
    ),
    CHECK (
        (state = 'planned' AND attempt_id IS NULL AND error_code IS NULL AND started_at IS NULL AND finished_at IS NULL) OR
        (state = 'in_flight' AND attempt_id IS NOT NULL AND error_code IS NULL AND started_at IS NOT NULL AND finished_at IS NULL) OR
        (state = 'applied' AND attempt_id IS NOT NULL AND error_code IS NULL AND started_at IS NOT NULL AND finished_at IS NOT NULL AND finished_at >= started_at) OR
        (state = 'not_started' AND attempt_id IS NOT NULL AND error_code IS NOT NULL AND error_code = 'not_started' AND started_at IS NOT NULL AND finished_at IS NOT NULL AND finished_at >= started_at) OR
        (state = 'unknown' AND attempt_id IS NOT NULL AND error_code IS NOT NULL AND error_code IN ('outcome_unknown', 'deadline_expired', 'owner_interrupted') AND started_at IS NOT NULL AND finished_at IS NOT NULL AND finished_at >= started_at)
    )
) ENGINE=InnoDB;

-- M8 irreversible one-shot host-action admission for the activation's probe_init
-- and service_start steps. It has the shape of probe_host_action_admissions
-- (086) but references the activation steps, so install admissions can never
-- consume an activation attempt and vice versa. No signature, private key,
-- token, raw output, applied state, renewal or rearm is stored here.
CREATE TABLE IF NOT EXISTS probe_activation_admissions (
    operation_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(operation_id) = 36 AND SUBSTRING(operation_id, 9, 1) = '-' AND SUBSTRING(operation_id, 14, 1) = '-' AND SUBSTRING(operation_id, 19, 1) = '-' AND SUBSTRING(operation_id, 24, 1) = '-' AND CAST(REPLACE(operation_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND operation_id <> '00000000-0000-0000-0000-000000000000'),
    owner_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(owner_id) = 36 AND SUBSTRING(owner_id, 9, 1) = '-' AND SUBSTRING(owner_id, 14, 1) = '-' AND SUBSTRING(owner_id, 19, 1) = '-' AND SUBSTRING(owner_id, 24, 1) = '-' AND CAST(REPLACE(owner_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND owner_id <> '00000000-0000-0000-0000-000000000000'),
    attempt_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(attempt_id) = 36 AND SUBSTRING(attempt_id, 9, 1) = '-' AND SUBSTRING(attempt_id, 14, 1) = '-' AND SUBSTRING(attempt_id, 19, 1) = '-' AND SUBSTRING(attempt_id, 24, 1) = '-' AND CAST(REPLACE(attempt_id, '-', '') AS BINARY) REGEXP '^[0-9a-f]{32}$' AND attempt_id <> '00000000-0000-0000-0000-000000000000'),
    kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (kind IN ('probe_init', 'service_start')),
    authority_key_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(authority_key_id) = 64 AND CAST(authority_key_id AS BINARY) REGEXP '^[0-9a-f]{64}$'),
    nonce_hex VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(nonce_hex) = 64 AND CAST(nonce_hex AS BINARY) REGEXP '^[0-9a-f]{64}$' AND nonce_hex <> '0000000000000000000000000000000000000000000000000000000000000000'),
    marker_sha256 VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(marker_sha256) = 64 AND CAST(marker_sha256 AS BINARY) REGEXP '^[0-9a-f]{64}$'),
    admitted_at DATETIME(6) NOT NULL,
    PRIMARY KEY (operation_id, kind),
    UNIQUE KEY uq_probe_activation_admission_attempt (attempt_id),
    UNIQUE KEY uq_probe_activation_admission_nonce (authority_key_id, nonce_hex),
    CONSTRAINT fk_probe_activation_admission_step FOREIGN KEY (operation_id, kind) REFERENCES probe_install_activation_steps (operation_id, kind) ON DELETE RESTRICT
) ENGINE=InnoDB;
