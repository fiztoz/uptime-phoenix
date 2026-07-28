CREATE TABLE users_new (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    username        TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    active          INTEGER NOT NULL DEFAULT 1,
    timezone        TEXT DEFAULT 'UTC',
    totp_secret     BLOB,
    totp_enabled    INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO users_new (id, username, password_hash, active, timezone, totp_secret, totp_enabled, created_at, updated_at)
    SELECT id, username, password_hash, active, timezone, totp_secret, totp_enabled, created_at, updated_at FROM users;
DROP TABLE users;
ALTER TABLE users_new RENAME TO users;
