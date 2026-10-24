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
-- authority lock and the FK below restricts its deletion. Booleans are stored
-- as explicit 0/1 integers with typeof guards.
CREATE TABLE IF NOT EXISTS probe_install_activations (
    operation_id TEXT NOT NULL PRIMARY KEY CHECK (length(operation_id) = 36),
    probe_key TEXT NOT NULL CHECK (length(probe_key) BETWEEN 1 AND 63 AND probe_key <> 'local'),
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    location TEXT NOT NULL CHECK (length(location) BETWEEN 0 AND 255),
    listen_port INTEGER NOT NULL CHECK (typeof(listen_port) = 'integer' AND listen_port BETWEEN 1024 AND 65535),
    client_key_fingerprint TEXT NOT NULL CHECK (client_key_fingerprint LIKE 'SHA256:%'),
    requested_by INTEGER NOT NULL CHECK (typeof(requested_by) = 'integer' AND requested_by > 0),
    owner_id TEXT NOT NULL CHECK (length(owner_id) = 36 AND substr(owner_id, 9, 1) = '-' AND substr(owner_id, 14, 1) = '-' AND substr(owner_id, 19, 1) = '-' AND substr(owner_id, 24, 1) = '-' AND length(replace(owner_id, '-', '')) = 32 AND replace(owner_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND owner_id <> '00000000-0000-0000-0000-000000000000'),
    state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'active', 'cancel_requested', 'failed', 'canceled', 'interrupted')),
    error_code TEXT NULL CHECK (error_code IS NULL OR error_code IN ('unsupported_host', 'artifact_rejected', 'resource_conflict', 'execution_failed', 'permission_denied', 'runtime_unverified', 'not_started', 'internal', 'canceled', 'owner_interrupted', 'deadline_expired', 'outcome_unknown')),
    probe_id TEXT NULL,
    stream_id TEXT NULL,
    certificate_fingerprint TEXT NULL,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    deadline_at TIMESTAMP NOT NULL,
    CHECK (deadline_at > created_at),
    CHECK (updated_at >= created_at),
    CHECK (
        (probe_id IS NULL AND stream_id IS NULL AND certificate_fingerprint IS NULL) OR
        (probe_id IS NOT NULL AND stream_id IS NOT NULL AND certificate_fingerprint IS NOT NULL
            AND length(probe_id) = 36 AND substr(probe_id, 9, 1) = '-' AND substr(probe_id, 14, 1) = '-' AND substr(probe_id, 19, 1) = '-' AND substr(probe_id, 24, 1) = '-' AND length(replace(probe_id, '-', '')) = 32 AND replace(probe_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND probe_id <> '00000000-0000-0000-0000-000000000000'
            AND length(stream_id) = 36 AND substr(stream_id, 9, 1) = '-' AND substr(stream_id, 14, 1) = '-' AND substr(stream_id, 19, 1) = '-' AND substr(stream_id, 24, 1) = '-' AND length(replace(stream_id, '-', '')) = 32 AND replace(stream_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND stream_id <> '00000000-0000-0000-0000-000000000000'
            AND length(certificate_fingerprint) = 64 AND certificate_fingerprint NOT GLOB '*[^0-9a-f]*')
    ),
    CHECK (state <> 'queued' OR probe_id IS NULL),
    CHECK (
        (state IN ('queued', 'running', 'active', 'cancel_requested') AND error_code IS NULL) OR
        (state = 'failed' AND error_code IS NOT NULL AND error_code IN ('unsupported_host', 'artifact_rejected', 'resource_conflict', 'execution_failed', 'permission_denied', 'runtime_unverified', 'not_started', 'internal')) OR
        (state = 'canceled' AND error_code IS NOT NULL AND error_code = 'canceled') OR
        (state = 'interrupted' AND error_code IS NOT NULL AND error_code IN ('owner_interrupted', 'deadline_expired', 'outcome_unknown'))
    ),
    FOREIGN KEY (operation_id) REFERENCES probe_install_operations (operation_id) ON DELETE RESTRICT
);

-- One immutable attempt per canonical step (init, registration, start,
-- enrollment). attempt_id is a unique attempt UUID; planned rows carry no
-- attempt residue at all. The FK restricts parent deletion.
CREATE TABLE IF NOT EXISTS probe_install_activation_steps (
    operation_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('probe_init', 'registration', 'service_start', 'enrollment')),
    ordinal INTEGER NOT NULL CHECK (typeof(ordinal) = 'integer' AND ordinal BETWEEN 1 AND 4),
    reference TEXT NOT NULL CHECK (length(reference) BETWEEN 1 AND 512),
    owned INTEGER NOT NULL CHECK (typeof(owned) = 'integer' AND owned IN (0, 1)),
    state TEXT NOT NULL CHECK (state IN ('planned', 'in_flight', 'applied', 'not_started', 'unknown')),
    attempt_id TEXT NULL CHECK (attempt_id IS NULL OR length(attempt_id) = 36),
    error_code TEXT NULL CHECK (error_code IS NULL OR error_code IN ('not_started', 'outcome_unknown', 'deadline_expired', 'owner_interrupted')),
    started_at TIMESTAMP NULL,
    finished_at TIMESTAMP NULL,
    PRIMARY KEY (operation_id, kind),
    UNIQUE (operation_id, ordinal),
    UNIQUE (attempt_id),
    FOREIGN KEY (operation_id) REFERENCES probe_install_activations (operation_id) ON DELETE RESTRICT,
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
);

-- M8 irreversible one-shot host-action admission for the activation's probe_init
-- and service_start steps. It has the shape of probe_host_action_admissions
-- (086) but references the activation steps, so install admissions can never
-- consume an activation attempt and vice versa. No signature, private key,
-- token, raw output, applied state, renewal or rearm is stored here.
CREATE TABLE IF NOT EXISTS probe_activation_admissions (
    operation_id TEXT NOT NULL CHECK (length(operation_id) = 36 AND substr(operation_id, 9, 1) = '-' AND substr(operation_id, 14, 1) = '-' AND substr(operation_id, 19, 1) = '-' AND substr(operation_id, 24, 1) = '-' AND length(replace(operation_id, '-', '')) = 32 AND replace(operation_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND operation_id <> '00000000-0000-0000-0000-000000000000'),
    owner_id TEXT NOT NULL CHECK (length(owner_id) = 36 AND substr(owner_id, 9, 1) = '-' AND substr(owner_id, 14, 1) = '-' AND substr(owner_id, 19, 1) = '-' AND substr(owner_id, 24, 1) = '-' AND length(replace(owner_id, '-', '')) = 32 AND replace(owner_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND owner_id <> '00000000-0000-0000-0000-000000000000'),
    attempt_id TEXT NOT NULL CHECK (length(attempt_id) = 36 AND substr(attempt_id, 9, 1) = '-' AND substr(attempt_id, 14, 1) = '-' AND substr(attempt_id, 19, 1) = '-' AND substr(attempt_id, 24, 1) = '-' AND length(replace(attempt_id, '-', '')) = 32 AND replace(attempt_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND attempt_id <> '00000000-0000-0000-0000-000000000000'),
    kind TEXT NOT NULL CHECK (kind IN ('probe_init', 'service_start')),
    authority_key_id TEXT NOT NULL CHECK (length(authority_key_id) = 64 AND authority_key_id NOT GLOB '*[^0-9a-f]*'),
    nonce_hex TEXT NOT NULL CHECK (length(nonce_hex) = 64 AND nonce_hex NOT GLOB '*[^0-9a-f]*' AND nonce_hex <> '0000000000000000000000000000000000000000000000000000000000000000'),
    marker_sha256 TEXT NOT NULL CHECK (length(marker_sha256) = 64 AND marker_sha256 NOT GLOB '*[^0-9a-f]*'),
    admitted_at TIMESTAMP NOT NULL,
    PRIMARY KEY (operation_id, kind),
    UNIQUE (attempt_id),
    UNIQUE (authority_key_id, nonce_hex),
    FOREIGN KEY (operation_id, kind) REFERENCES probe_install_activation_steps (operation_id, kind) ON DELETE RESTRICT
);
