CREATE TABLE webauthn_credentials_old (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL,
    credential_id   TEXT NOT NULL UNIQUE,
    public_key      TEXT NOT NULL,
    sign_count      INTEGER NOT NULL DEFAULT 0,
    transports      TEXT NOT NULL DEFAULT '[]',
    name            TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at    TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

INSERT INTO webauthn_credentials_old (
    id, user_id, credential_id, public_key, sign_count, transports, name, created_at, last_used_at
)
SELECT id, user_id, credential_id, public_key, sign_count, transports, name, created_at, last_used_at
FROM webauthn_credentials;

DROP TABLE webauthn_credentials;
ALTER TABLE webauthn_credentials_old RENAME TO webauthn_credentials;
CREATE INDEX idx_webauthn_user ON webauthn_credentials (user_id);
