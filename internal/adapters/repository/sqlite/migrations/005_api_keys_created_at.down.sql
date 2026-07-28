CREATE TABLE api_keys_new (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL,
    name        TEXT NOT NULL,
    key_hash    TEXT NOT NULL UNIQUE,
    active      INTEGER NOT NULL DEFAULT 1,
    expires_at  TEXT,
    scopes      TEXT NOT NULL DEFAULT '[\"read\"]',
    last_used_at TEXT,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);
INSERT INTO api_keys_new (id, user_id, name, key_hash, active, expires_at, scopes, last_used_at)
    SELECT id, user_id, name, key_hash, active, expires_at, scopes, last_used_at FROM api_keys;
DROP TABLE api_keys;
ALTER TABLE api_keys_new RENAME TO api_keys;
