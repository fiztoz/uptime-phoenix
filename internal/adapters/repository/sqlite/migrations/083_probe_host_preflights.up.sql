-- M8 durable asynchronous SSH host-preflight discovery receipts. Receipts hold
-- only frozen request metadata, PUBLIC key fingerprints, actor/owner IDs, fixed
-- error codes and supported compatibility facts. No private key, key-file
-- path, token, password or raw SSH output column exists here by construction.
-- The owner UUID fences every write in the database; the 90-second deadline is
-- a fixed DB-clock bound with no renewal, takeover or epoch reuse.
CREATE TABLE IF NOT EXISTS probe_host_preflights (
    operation_id TEXT NOT NULL PRIMARY KEY CHECK (length(operation_id) = 36),
    ssh_address TEXT NOT NULL,
    ssh_user TEXT NOT NULL CHECK (length(ssh_user) BETWEEN 1 AND 64),
    host_key_fingerprint TEXT NOT NULL CHECK (host_key_fingerprint LIKE 'SHA256:%'),
    client_key_fingerprint TEXT NOT NULL CHECK (client_key_fingerprint LIKE 'SHA256:%'),
    requested_by INTEGER NOT NULL CHECK (typeof(requested_by) = 'integer' AND requested_by > 0),
    owner_id TEXT NOT NULL CHECK (length(owner_id) = 36),
    state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'cancel_requested', 'preflight_passed', 'failed', 'canceled', 'interrupted')),
    error_code TEXT NULL CHECK (error_code IS NULL OR error_code IN ('ssh_trust', 'ssh_destination', 'ssh_connect', 'ssh_discovery', 'unsupported_platform', 'missing_privilege', 'missing_runtime', 'timeout', 'internal', 'canceled', 'interrupted')),
    result_os TEXT NULL,
    result_architecture TEXT NULL,
    result_docker_cli INTEGER NULL CHECK (result_docker_cli IS NULL OR (typeof(result_docker_cli) = 'integer' AND result_docker_cli IN (0, 1))),
    result_systemd INTEGER NULL CHECK (result_systemd IS NULL OR (typeof(result_systemd) = 'integer' AND result_systemd IN (0, 1))),
    result_privilege TEXT NULL,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    deadline_at TIMESTAMP NOT NULL,
    CHECK (deadline_at > created_at),
    CHECK (updated_at >= created_at),
    CHECK (
        (state IN ('queued', 'running', 'cancel_requested') AND error_code IS NULL AND result_os IS NULL AND result_architecture IS NULL AND result_docker_cli IS NULL AND result_systemd IS NULL AND result_privilege IS NULL) OR
        (state = 'preflight_passed' AND error_code IS NULL AND result_os IS NOT NULL AND result_architecture IS NOT NULL AND result_docker_cli IS NOT NULL AND result_systemd IS NOT NULL AND result_privilege IS NOT NULL AND result_os = 'linux' AND result_architecture IN ('amd64', 'arm64') AND result_privilege IN ('root', 'sudo') AND (result_docker_cli = 1 OR result_systemd = 1)) OR
        (state = 'failed' AND error_code IS NOT NULL AND error_code IN ('ssh_trust', 'ssh_destination', 'ssh_connect', 'ssh_discovery', 'unsupported_platform', 'missing_privilege', 'missing_runtime', 'timeout', 'internal') AND result_os IS NULL AND result_architecture IS NULL AND result_docker_cli IS NULL AND result_systemd IS NULL AND result_privilege IS NULL) OR
        (state = 'canceled' AND error_code IS NOT NULL AND error_code = 'canceled' AND result_os IS NULL AND result_architecture IS NULL AND result_docker_cli IS NULL AND result_systemd IS NULL AND result_privilege IS NULL) OR
        (state = 'interrupted' AND error_code IS NOT NULL AND error_code = 'interrupted' AND result_os IS NULL AND result_architecture IS NULL AND result_docker_cli IS NULL AND result_systemd IS NULL AND result_privilege IS NULL)
    )
);
