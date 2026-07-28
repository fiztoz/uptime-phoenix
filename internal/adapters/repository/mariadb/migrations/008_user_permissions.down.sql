-- 008_user_permissions.down.sql — MariaDB: revert per-user RBAC grants
-- Created_at: 2026-07-12
-- Updated_at: 2026-07-12
--
-- Dropping the table drops its three foreign keys with it, so unlike 007 there
-- is no DROP FOREIGN KEY dance to get right here.

DROP TABLE IF EXISTS user_permissions;

ALTER TABLE users
    DROP COLUMN IF EXISTS can_manage_notifications,
    DROP COLUMN IF EXISTS can_manage_maintenance;
