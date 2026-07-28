-- WebAuthn (passkey) credentials registered by users.
-- Created_at: 2026-06-28
-- Updated_at: 2026-06-28
--
-- credential_id and public_key are stored as base64url TEXT so the same
-- adapter code works on both SQLite and MariaDB without engine-specific BLOB
-- handling. transports is a JSON array.

CREATE TABLE webauthn_credentials (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL,
    credential_id   TEXT NOT NULL UNIQUE,    -- base64url(raw credential id)
    public_key      TEXT NOT NULL,           -- base64url(COSE public key)
    sign_count      INTEGER NOT NULL DEFAULT 0,
    transports      TEXT NOT NULL DEFAULT '[]',
    name            TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at    TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX idx_webauthn_user ON webauthn_credentials (user_id);
