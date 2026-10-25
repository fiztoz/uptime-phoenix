-- M8 durable installation intent and resource journal (layout v1). Rows hold
-- only frozen request metadata, PUBLIC key fingerprints, actor/owner/admission
-- IDs, requested artifact pins, canonical resource references and fixed state/
-- error codes. No private key, key-file path, token, password, raw remote
-- output or installed-success state exists here by construction.
-- The owner UUID fences every write in the database; the 15-minute deadline is
-- a fixed DB-clock bound with no renewal, takeover, resume or epoch reuse.
-- Every operation carries an internal create-admission UUID that is minted per
-- transaction attempt and stored so a fresh creation is proven by comparison
-- under the row lock, never by affected-row counts. Booleans are stored as
-- explicit 0/1 integers with typeof guards.
CREATE TABLE IF NOT EXISTS probe_install_operations (
    operation_id TEXT NOT NULL PRIMARY KEY CHECK (length(operation_id) = 36),
    ssh_address TEXT NOT NULL,
    ssh_user TEXT NOT NULL CHECK (length(ssh_user) BETWEEN 1 AND 64),
    host_key_fingerprint TEXT NOT NULL CHECK (host_key_fingerprint LIKE 'SHA256:%'),
    client_key_fingerprint TEXT NOT NULL CHECK (client_key_fingerprint LIKE 'SHA256:%'),
    requested_by INTEGER NOT NULL CHECK (typeof(requested_by) = 'integer' AND requested_by > 0),
    layout_version INTEGER NOT NULL CHECK (typeof(layout_version) = 'integer' AND layout_version = 1),
    mode TEXT NOT NULL CHECK (mode IN ('systemd', 'docker')),
    binary_version TEXT NULL,
    binary_architecture TEXT NULL,
    binary_sha256 TEXT NULL,
    binary_size INTEGER NULL,
    image_repository TEXT NULL,
    image_version TEXT NULL,
    image_architecture TEXT NULL,
    image_index_sha256 TEXT NULL,
    image_index_size INTEGER NULL,
    image_manifest_sha256 TEXT NULL,
    image_config_sha256 TEXT NULL,
    image_layer_count INTEGER NULL,
    owner_id TEXT NOT NULL CHECK (length(owner_id) = 36),
    admission_id TEXT NOT NULL CHECK (length(admission_id) = 36),
    state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'resources_ready', 'cancel_requested', 'failed', 'canceled', 'interrupted')),
    error_code TEXT NULL CHECK (error_code IS NULL OR error_code IN ('unsupported_host', 'artifact_rejected', 'resource_conflict', 'execution_failed', 'permission_denied', 'runtime_unverified', 'not_started', 'internal', 'canceled', 'owner_interrupted', 'deadline_expired', 'outcome_unknown')),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    deadline_at TIMESTAMP NOT NULL,
    CHECK (deadline_at > created_at),
    CHECK (updated_at >= created_at),
    CHECK (binary_size IS NULL OR (typeof(binary_size) = 'integer' AND binary_size > 0)),
    CHECK (image_index_size IS NULL OR (typeof(image_index_size) = 'integer' AND image_index_size > 0)),
    CHECK (image_layer_count IS NULL OR (typeof(image_layer_count) = 'integer' AND image_layer_count >= 1)),
    CHECK (
        (mode = 'systemd' AND binary_version IS NOT NULL AND binary_architecture IS NOT NULL AND binary_sha256 IS NOT NULL AND binary_size IS NOT NULL
            AND image_repository IS NULL AND image_version IS NULL AND image_architecture IS NULL AND image_index_sha256 IS NULL AND image_index_size IS NULL AND image_manifest_sha256 IS NULL AND image_config_sha256 IS NULL AND image_layer_count IS NULL) OR
        (mode = 'docker' AND image_repository IS NOT NULL AND image_version IS NOT NULL AND image_architecture IS NOT NULL AND image_index_sha256 IS NOT NULL AND image_index_size IS NOT NULL AND image_manifest_sha256 IS NOT NULL AND image_config_sha256 IS NOT NULL AND image_layer_count IS NOT NULL
            AND binary_version IS NULL AND binary_architecture IS NULL AND binary_sha256 IS NULL AND binary_size IS NULL)
    ),
    CHECK (
        (state IN ('queued', 'running', 'resources_ready', 'cancel_requested') AND error_code IS NULL) OR
        (state = 'failed' AND error_code IS NOT NULL AND error_code IN ('unsupported_host', 'artifact_rejected', 'resource_conflict', 'execution_failed', 'permission_denied', 'runtime_unverified', 'not_started', 'internal')) OR
        (state = 'canceled' AND error_code IS NOT NULL AND error_code = 'canceled') OR
        (state = 'interrupted' AND error_code IS NOT NULL AND error_code IN ('owner_interrupted', 'deadline_expired', 'outcome_unknown'))
    )
);

-- One immutable attempt per canonical resource. attempt_id is a globally
-- unique attempt UUID; planned rows carry no attempt residue at all. The FK
-- restricts parent deletion: a downgrade must drop children explicitly.
CREATE TABLE IF NOT EXISTS probe_install_resources (
    operation_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (typeof(ordinal) = 'integer' AND ordinal >= 1),
    kind TEXT NOT NULL CHECK (kind IN ('data_directory', 'artifact_directory', 'unit_file', 'service', 'image_cache', 'container')),
    reference TEXT NOT NULL CHECK (length(reference) BETWEEN 1 AND 512),
    owned INTEGER NOT NULL CHECK (typeof(owned) = 'integer' AND owned IN (0, 1)),
    state TEXT NOT NULL CHECK (state IN ('planned', 'in_flight', 'applied', 'not_started', 'unknown')),
    attempt_id TEXT NULL CHECK (attempt_id IS NULL OR length(attempt_id) = 36),
    error_code TEXT NULL CHECK (error_code IS NULL OR error_code IN ('not_started', 'outcome_unknown', 'deadline_expired', 'owner_interrupted')),
    started_at TIMESTAMP NULL,
    finished_at TIMESTAMP NULL,
    PRIMARY KEY (operation_id, ordinal),
    UNIQUE (operation_id, kind),
    UNIQUE (attempt_id),
    FOREIGN KEY (operation_id) REFERENCES probe_install_operations (operation_id) ON DELETE RESTRICT,
    CHECK (
        (state = 'planned' AND attempt_id IS NULL AND error_code IS NULL AND started_at IS NULL AND finished_at IS NULL) OR
        (state = 'in_flight' AND attempt_id IS NOT NULL AND error_code IS NULL AND started_at IS NOT NULL AND finished_at IS NULL) OR
        (state = 'applied' AND attempt_id IS NOT NULL AND error_code IS NULL AND started_at IS NOT NULL AND finished_at IS NOT NULL AND finished_at >= started_at) OR
        (state = 'not_started' AND attempt_id IS NOT NULL AND error_code IS NOT NULL AND error_code = 'not_started' AND started_at IS NOT NULL AND finished_at IS NOT NULL AND finished_at >= started_at) OR
        (state = 'unknown' AND attempt_id IS NOT NULL AND error_code IS NOT NULL AND error_code IN ('outcome_unknown', 'deadline_expired', 'owner_interrupted') AND started_at IS NOT NULL AND finished_at IS NOT NULL AND finished_at >= started_at)
    )
);
