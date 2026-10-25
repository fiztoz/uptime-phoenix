-- M8 irreversible one-shot host-action admission for data_directory, unit_file
-- and service. A new table leaves 085 untouched (SQLite cannot alter CHECKs) and
-- artifact_directory stays in probe_binary_stage_admissions. No signature, private
-- key, token, raw output, applied state, renewal or rearm is stored here.
CREATE TABLE IF NOT EXISTS probe_host_action_admissions (
    operation_id TEXT NOT NULL CHECK (length(operation_id) = 36 AND substr(operation_id, 9, 1) = '-' AND substr(operation_id, 14, 1) = '-' AND substr(operation_id, 19, 1) = '-' AND substr(operation_id, 24, 1) = '-' AND length(replace(operation_id, '-', '')) = 32 AND replace(operation_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND operation_id <> '00000000-0000-0000-0000-000000000000'),
    owner_id TEXT NOT NULL CHECK (length(owner_id) = 36 AND substr(owner_id, 9, 1) = '-' AND substr(owner_id, 14, 1) = '-' AND substr(owner_id, 19, 1) = '-' AND substr(owner_id, 24, 1) = '-' AND length(replace(owner_id, '-', '')) = 32 AND replace(owner_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND owner_id <> '00000000-0000-0000-0000-000000000000'),
    attempt_id TEXT NOT NULL CHECK (length(attempt_id) = 36 AND substr(attempt_id, 9, 1) = '-' AND substr(attempt_id, 14, 1) = '-' AND substr(attempt_id, 19, 1) = '-' AND substr(attempt_id, 24, 1) = '-' AND length(replace(attempt_id, '-', '')) = 32 AND replace(attempt_id, '-', '') NOT GLOB '*[^0-9a-f]*' AND attempt_id <> '00000000-0000-0000-0000-000000000000'),
    kind TEXT NOT NULL CHECK (kind IN ('data_directory', 'unit_file', 'service')),
    authority_key_id TEXT NOT NULL CHECK (length(authority_key_id) = 64 AND authority_key_id NOT GLOB '*[^0-9a-f]*'),
    nonce_hex TEXT NOT NULL CHECK (length(nonce_hex) = 64 AND nonce_hex NOT GLOB '*[^0-9a-f]*' AND nonce_hex <> '0000000000000000000000000000000000000000000000000000000000000000'),
    marker_sha256 TEXT NOT NULL CHECK (length(marker_sha256) = 64 AND marker_sha256 NOT GLOB '*[^0-9a-f]*'),
    admitted_at TIMESTAMP NOT NULL,
    PRIMARY KEY (operation_id, kind),
    UNIQUE (attempt_id),
    UNIQUE (authority_key_id, nonce_hex),
    FOREIGN KEY (operation_id, kind) REFERENCES probe_install_resources (operation_id, kind) ON DELETE RESTRICT
);
