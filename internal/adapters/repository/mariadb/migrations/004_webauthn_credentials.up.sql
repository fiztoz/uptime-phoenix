-- WebAuthn (passkey) credentials registered by users.
-- Created_at: 2026-06-28
-- Updated_at: 2026-06-28
--
-- credential_id and public_key are stored as base64url TEXT so the same
-- adapter code works on both MariaDB and SQLite without engine-specific BLOB
-- handling. transports is a JSON array.

CREATE TABLE webauthn_credentials (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id         BIGINT NOT NULL,
    credential_id   VARCHAR(512) NOT NULL,   -- base64url(raw credential id)
    public_key      TEXT NOT NULL,           -- base64url(COSE public key)
    sign_count      INT UNSIGNED NOT NULL DEFAULT 0,
    transports      TEXT NOT NULL DEFAULT ('[]'),
    name            VARCHAR(255) NOT NULL DEFAULT '',
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at    TIMESTAMP NULL DEFAULT NULL,
    UNIQUE KEY uq_webauthn_cred_id (credential_id),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    INDEX idx_webauthn_user (user_id)
);
