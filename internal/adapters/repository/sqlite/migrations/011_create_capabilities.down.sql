-- 011_create_capabilities.down.sql — SQLite: revert creation capabilities + shallow group grants
-- Created_at: 2026-07-17
-- Updated_at: 2026-07-17
--
-- Both tables are rebuilt rather than ALTERed in place, following 006's and
-- 008's downs: SQLite only learned DROP COLUMN in 3.35 and the repo does not
-- pin a floor that high, so a rebuild is the portable move.
--
-- Order matters. user_permissions is rebuilt FIRST, while the current users
-- table is still present, so its FK to users(id) always has a target. The users
-- rebuild that follows then repoints that FK at the recreated users table on
-- rename, the same way 008's down relies on.
--
-- Reverting is lossy in exactly one way, and it is the safe direction: a shallow
-- grant (include_descendants = 0) becomes recursive again, because 008-era code
-- has no way to express "this folder but not its subfolders". That WIDENS the
-- affected users' visibility back to what the older schema would always have
-- given them. Anyone relying on a shallow grant to withhold a subtree must
-- re-check those users after a downgrade — the narrowing is not recoverable
-- from the 008 schema alone.
--
-- users_old drops only the two columns 011 added and keeps the 008 capability
-- columns; 009 and 010 add nothing to users, so this is the shape at the end
-- of 010.

CREATE TABLE user_permissions_old (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL,
    monitor_id  INTEGER,
    group_id    INTEGER,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (group_id) REFERENCES monitor_groups(id) ON DELETE CASCADE,
    CHECK ((monitor_id IS NULL) <> (group_id IS NULL))
);

INSERT INTO user_permissions_old (id, user_id, monitor_id, group_id, created_at)
    SELECT id, user_id, monitor_id, group_id, created_at FROM user_permissions;

DROP TABLE user_permissions;
ALTER TABLE user_permissions_old RENAME TO user_permissions;

CREATE UNIQUE INDEX idx_user_permissions_user_monitor ON user_permissions(user_id, monitor_id);
CREATE UNIQUE INDEX idx_user_permissions_user_group ON user_permissions(user_id, group_id);
CREATE INDEX idx_user_permissions_user ON user_permissions(user_id);

CREATE TABLE users_old (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    username                 TEXT NOT NULL UNIQUE,
    password_hash            TEXT NOT NULL,
    active                   INTEGER NOT NULL DEFAULT 1,
    is_admin                 INTEGER NOT NULL DEFAULT 0,
    can_manage_notifications INTEGER NOT NULL DEFAULT 0,
    can_manage_maintenance   INTEGER NOT NULL DEFAULT 0,
    timezone                 TEXT DEFAULT 'UTC',
    totp_secret              BLOB,
    totp_enabled             INTEGER NOT NULL DEFAULT 0,
    created_at               TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at               TEXT NOT NULL DEFAULT (datetime('now'))
);

INSERT INTO users_old (id, username, password_hash, active, is_admin, can_manage_notifications, can_manage_maintenance, timezone, totp_secret, totp_enabled, created_at, updated_at)
    SELECT id, username, password_hash, active, is_admin, can_manage_notifications, can_manage_maintenance, timezone, totp_secret, totp_enabled, created_at, updated_at FROM users;

DROP TABLE users;
ALTER TABLE users_old RENAME TO users;
