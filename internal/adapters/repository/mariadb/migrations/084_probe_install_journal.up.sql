-- M8 durable installation intent and resource journal (layout v1). Rows hold
-- only frozen request metadata, PUBLIC key fingerprints, actor/owner/admission
-- IDs, requested artifact pins, canonical resource references and fixed state/
-- error codes. No private key, key-file path, token, password, raw remote
-- output or installed-success state exists here by construction.
-- The owner UUID fences every write in the database; the 15-minute deadline is
-- a fixed DB-clock bound with no renewal, takeover, resume or epoch reuse.
-- Every operation carries an internal create-admission UUID that is minted per
-- transaction attempt and stored so a fresh creation is proven by comparison
-- under the row lock, never by affected-row counts.
CREATE TABLE IF NOT EXISTS probe_install_operations (
    operation_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY CHECK (CHAR_LENGTH(operation_id) = 36),
    ssh_address VARCHAR(260) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    ssh_user VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(ssh_user) BETWEEN 1 AND 64),
    host_key_fingerprint VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (host_key_fingerprint LIKE 'SHA256:%'),
    client_key_fingerprint VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (client_key_fingerprint LIKE 'SHA256:%'),
    requested_by BIGINT NOT NULL CHECK (requested_by > 0),
    layout_version SMALLINT NOT NULL CHECK (layout_version = 1),
    mode VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (mode IN ('systemd', 'docker')),
    binary_version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    binary_architecture VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL,
    binary_sha256 VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    binary_size BIGINT NULL,
    image_repository VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL,
    image_version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    image_architecture VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL,
    image_index_sha256 VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    image_index_size BIGINT NULL,
    image_manifest_sha256 VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    image_config_sha256 VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    image_layer_count INT NULL,
    owner_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(owner_id) = 36),
    admission_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(admission_id) = 36),
    state VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (state IN ('queued', 'running', 'resources_ready', 'cancel_requested', 'failed', 'canceled', 'interrupted')),
    error_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL CHECK (error_code IS NULL OR error_code IN ('unsupported_host', 'artifact_rejected', 'resource_conflict', 'execution_failed', 'permission_denied', 'runtime_unverified', 'not_started', 'internal', 'canceled', 'owner_interrupted', 'deadline_expired', 'outcome_unknown')),
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    deadline_at DATETIME(6) NOT NULL,
    CHECK (deadline_at > created_at),
    CHECK (updated_at >= created_at),
    CHECK (binary_size IS NULL OR binary_size > 0),
    CHECK (image_index_size IS NULL OR image_index_size > 0),
    CHECK (image_layer_count IS NULL OR image_layer_count >= 1),
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
) ENGINE=InnoDB;

-- One immutable attempt per canonical resource. attempt_id is a globally
-- unique attempt UUID; planned rows carry no attempt residue at all. The FK
-- restricts parent deletion: a downgrade must drop children explicitly.
CREATE TABLE IF NOT EXISTS probe_install_resources (
    operation_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    ordinal INT NOT NULL CHECK (ordinal >= 1),
    kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (kind IN ('data_directory', 'artifact_directory', 'unit_file', 'service', 'image_cache', 'container')),
    reference VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(reference) BETWEEN 1 AND 512),
    owned TINYINT(1) NOT NULL CHECK (owned IN (0, 1)),
    state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (state IN ('planned', 'in_flight', 'applied', 'not_started', 'unknown')),
    attempt_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL CHECK (attempt_id IS NULL OR CHAR_LENGTH(attempt_id) = 36),
    error_code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL CHECK (error_code IS NULL OR error_code IN ('not_started', 'outcome_unknown', 'deadline_expired', 'owner_interrupted')),
    started_at DATETIME(6) NULL,
    finished_at DATETIME(6) NULL,
    PRIMARY KEY (operation_id, ordinal),
    UNIQUE KEY uq_probe_install_resources_kind (operation_id, kind),
    UNIQUE KEY uq_probe_install_resources_attempt (attempt_id),
    CONSTRAINT fk_probe_install_resources_operation FOREIGN KEY (operation_id) REFERENCES probe_install_operations (operation_id) ON DELETE RESTRICT,
    CHECK (
        (state = 'planned' AND attempt_id IS NULL AND error_code IS NULL AND started_at IS NULL AND finished_at IS NULL) OR
        (state = 'in_flight' AND attempt_id IS NOT NULL AND error_code IS NULL AND started_at IS NOT NULL AND finished_at IS NULL) OR
        (state = 'applied' AND attempt_id IS NOT NULL AND error_code IS NULL AND started_at IS NOT NULL AND finished_at IS NOT NULL AND finished_at >= started_at) OR
        (state = 'not_started' AND attempt_id IS NOT NULL AND error_code IS NOT NULL AND error_code = 'not_started' AND started_at IS NOT NULL AND finished_at IS NOT NULL AND finished_at >= started_at) OR
        (state = 'unknown' AND attempt_id IS NOT NULL AND error_code IS NOT NULL AND error_code IN ('outcome_unknown', 'deadline_expired', 'owner_interrupted') AND started_at IS NOT NULL AND finished_at IS NOT NULL AND finished_at >= started_at)
    )
) ENGINE=InnoDB;
