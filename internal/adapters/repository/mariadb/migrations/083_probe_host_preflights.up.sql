-- M8 durable asynchronous SSH host-preflight discovery receipts. Receipts hold
-- only frozen request metadata, PUBLIC key fingerprints, actor/owner IDs, fixed
-- error codes and supported compatibility facts. No private key, key-file
-- path, token, password or raw SSH output column exists here by construction.
-- The owner UUID fences every write in the database; the 90-second deadline is
-- a fixed DB-clock bound with no renewal, takeover or epoch reuse.
CREATE TABLE IF NOT EXISTS probe_host_preflights (
    operation_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY CHECK (CHAR_LENGTH(operation_id) = 36),
    ssh_address VARCHAR(260) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    ssh_user VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(ssh_user) BETWEEN 1 AND 64),
    host_key_fingerprint VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (host_key_fingerprint LIKE 'SHA256:%'),
    client_key_fingerprint VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (client_key_fingerprint LIKE 'SHA256:%'),
    requested_by BIGINT NOT NULL CHECK (requested_by > 0),
    owner_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(owner_id) = 36),
    state VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (state IN ('queued', 'running', 'cancel_requested', 'preflight_passed', 'failed', 'canceled', 'interrupted')),
    error_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL CHECK (error_code IS NULL OR error_code IN ('ssh_trust', 'ssh_destination', 'ssh_connect', 'ssh_discovery', 'unsupported_platform', 'missing_privilege', 'missing_runtime', 'timeout', 'internal', 'canceled', 'interrupted')),
    result_os VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL,
    result_architecture VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL,
    result_docker_cli TINYINT(1) NULL CHECK (result_docker_cli IS NULL OR result_docker_cli IN (0, 1)),
    result_systemd TINYINT(1) NULL CHECK (result_systemd IS NULL OR result_systemd IN (0, 1)),
    result_privilege VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    deadline_at DATETIME(6) NOT NULL,
    CHECK (deadline_at > created_at),
    CHECK (updated_at >= created_at),
    CHECK (
        (state IN ('queued', 'running', 'cancel_requested') AND error_code IS NULL AND result_os IS NULL AND result_architecture IS NULL AND result_docker_cli IS NULL AND result_systemd IS NULL AND result_privilege IS NULL) OR
        (state = 'preflight_passed' AND error_code IS NULL AND result_os IS NOT NULL AND result_architecture IS NOT NULL AND result_docker_cli IS NOT NULL AND result_systemd IS NOT NULL AND result_privilege IS NOT NULL AND result_os = 'linux' AND result_architecture IN ('amd64', 'arm64') AND result_privilege IN ('root', 'sudo') AND (result_docker_cli = 1 OR result_systemd = 1)) OR
        (state = 'failed' AND error_code IS NOT NULL AND error_code IN ('ssh_trust', 'ssh_destination', 'ssh_connect', 'ssh_discovery', 'unsupported_platform', 'missing_privilege', 'missing_runtime', 'timeout', 'internal') AND result_os IS NULL AND result_architecture IS NULL AND result_docker_cli IS NULL AND result_systemd IS NULL AND result_privilege IS NULL) OR
        (state = 'canceled' AND error_code IS NOT NULL AND error_code = 'canceled' AND result_os IS NULL AND result_architecture IS NULL AND result_docker_cli IS NULL AND result_systemd IS NULL AND result_privilege IS NULL) OR
        (state = 'interrupted' AND error_code IS NOT NULL AND error_code = 'interrupted' AND result_os IS NULL AND result_architecture IS NULL AND result_docker_cli IS NULL AND result_systemd IS NULL AND result_privilege IS NULL)
    )
) ENGINE=InnoDB;
