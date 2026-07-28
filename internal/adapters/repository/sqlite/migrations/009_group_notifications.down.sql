-- 009_group_notifications.down.sql — SQLite: revert group notifications
-- Created_at: 2026-07-13
-- Updated_at: 2026-07-13
--
-- group_notifications is dropped outright: it holds only links, and the up
-- migration created it from nothing.
--
-- monitor_groups is rebuilt rather than ALTERed, matching the pattern 007/008
-- already use in this adapter. SQLite's DROP COLUMN would work here (last_status
-- carries no constraint), but monitor_groups is the target of a self FOREIGN KEY
-- (parent_id) and of user_permissions.group_id / group_notifications.group_id,
-- and the rebuild keeps this file in the same shape as its neighbours. SQLite
-- repoints every other table's FOREIGN KEY ... REFERENCES monitor_groups(id) at
-- the recreated table once it is renamed back.
--
-- group_notifications is dropped FIRST so its FK to monitor_groups(id) cannot
-- object to the rebuild.

DROP TABLE IF EXISTS group_notifications;

CREATE TABLE monitor_groups_old (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id               INTEGER NOT NULL,
    name                  TEXT NOT NULL,
    description           TEXT,
    parent_id             INTEGER,
    status_condition      TEXT NOT NULL DEFAULT 'worst_of_children',
    threshold             INTEGER NOT NULL DEFAULT 0,
    threshold_is_percent  INTEGER NOT NULL DEFAULT 0,
    weight                INTEGER NOT NULL DEFAULT 2000,
    collapsed             INTEGER NOT NULL DEFAULT 0,
    created_at            TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at            TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (parent_id) REFERENCES monitor_groups_old(id) ON DELETE SET NULL
);

INSERT INTO monitor_groups_old (id, user_id, name, description, parent_id, status_condition, threshold, threshold_is_percent, weight, collapsed, created_at, updated_at)
    SELECT id, user_id, name, description, parent_id, status_condition, threshold, threshold_is_percent, weight, collapsed, created_at, updated_at FROM monitor_groups;

DROP TABLE monitor_groups;
ALTER TABLE monitor_groups_old RENAME TO monitor_groups;

CREATE INDEX idx_monitor_groups_user ON monitor_groups(user_id);
CREATE INDEX idx_monitor_groups_parent ON monitor_groups(parent_id);
