-- 008_user_permissions.up.sql — SQLite: per-user RBAC grants + capability flags
-- Created_at: 2026-07-12
-- Updated_at: 2026-07-12
--
-- Two independent pieces:
--
-- 1. users.can_manage_notifications / users.can_manage_maintenance — capability
--    flags a non-admin can hold. They are the ONLY write powers a non-admin can
--    be given; monitors and groups stay admin-only to mutate. Default 0 (false)
--    so every pre-existing non-admin user stays read-only. Pre-existing admins
--    are unaffected: IsAdmin already implies both capabilities in the service,
--    so no backfill is needed here (contrast 006, which had to promote existing
--    users to admin to avoid locking installs out of user management).
--
-- 2. user_permissions — the view grants themselves. One row grants ONE user
--    view access to ONE resource: a monitor (monitor_id set) or a group
--    (group_id set), never both. A group grant is recursive over the group tree
--    (expanded in AccessService, not here).
--
-- The CHECK enforces the exactly-one-target rule at the DB level so a bad grant
-- cannot be written even if a future caller bypasses the domain guard.
--
-- The two UNIQUE indexes make a grant idempotent per (user, resource). SQLite
-- treats NULLs as distinct in a UNIQUE index, so the many group-grant rows
-- (monitor_id NULL) for one user do not collide on
-- idx_user_permissions_user_monitor, and vice versa — this is exactly the
-- behavior we want and it matches InnoDB's.
--
-- Both FKs cascade on delete: deleting a monitor or a group must not leave a
-- dangling grant behind that a later monitor reusing the same autoincrement id
-- would silently inherit.

ALTER TABLE users ADD COLUMN can_manage_notifications INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN can_manage_maintenance INTEGER NOT NULL DEFAULT 0;

CREATE TABLE user_permissions (
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

CREATE UNIQUE INDEX idx_user_permissions_user_monitor ON user_permissions(user_id, monitor_id);
CREATE UNIQUE INDEX idx_user_permissions_user_group ON user_permissions(user_id, group_id);
CREATE INDEX idx_user_permissions_user ON user_permissions(user_id);
