-- 008_user_permissions.down.sql — SQLite: revert per-user RBAC grants
-- Created_at: 2026-07-12
-- Updated_at: 2026-07-12
--
-- users is rebuilt rather than ALTERed in place, exactly as 006's down does:
-- the recreated table keeps every column the schema had at the end of 006
-- (including is_admin) and drops only the two capability columns 008 added.
-- SQLite repoints other tables' FOREIGN KEY ... REFERENCES users(id) at the
-- recreated table once it is renamed back to "users".
--
-- user_permissions is dropped first so its FK to users(id) cannot object to the
-- rebuild.

DROP TABLE IF EXISTS user_permissions;

CREATE TABLE users_old (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    username        TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    active          INTEGER NOT NULL DEFAULT 1,
    is_admin        INTEGER NOT NULL DEFAULT 0,
    timezone        TEXT DEFAULT 'UTC',
    totp_secret     BLOB,
    totp_enabled    INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

INSERT INTO users_old (id, username, password_hash, active, is_admin, timezone, totp_secret, totp_enabled, created_at, updated_at)
    SELECT id, username, password_hash, active, is_admin, timezone, totp_secret, totp_enabled, created_at, updated_at FROM users;

DROP TABLE users;
ALTER TABLE users_old RENAME TO users;
