-- 011_create_capabilities.up.sql — MariaDB: creation capabilities + shallow group grants
-- Created_at: 2026-07-17
-- Updated_at: 2026-07-17
--
-- MariaDB twin of the SQLite 011. See that file for the full rationale; the
-- model is identical and only the types differ (BOOLEAN, which InnoDB stores as
-- TINYINT(1), against SQLite's INTEGER).
--
-- 1. users.can_create_monitors / users.can_create_groups — capability flags in
--    the same family as 008's can_manage_notifications / can_manage_maintenance.
--    A non-admin holding one may CREATE that resource type, and may edit and
--    delete what they themselves created — never what someone else created.
--    Ownership comes from the existing monitors.user_id / monitor_groups.user_id;
--    no new column is needed for it.
--
--    Default FALSE: this migration grants nobody anything.
--
-- 2. user_permissions.include_descendants — makes a GROUP grant's recursion a
--    per-grant choice. Default TRUE REPLICATES THE PRE-MIGRATION BEHAVIOR (008
--    group grants always cascaded); every existing row must keep cascading or
--    users lose visibility on upgrade. Never change this default to FALSE.
--
--      TRUE  (deep, default): the group, its descendant subgroups, and every
--                             monitor filed under any of them.
--      FALSE (shallow):       the group and the monitors filed DIRECTLY in it.
--                             Subgroups and their contents stay invisible.
--
--    Meaningless on a monitor grant — a monitor has no descendants — and the
--    service reads it only on the group branch.

ALTER TABLE users
    ADD COLUMN can_create_monitors BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN can_create_groups BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE user_permissions
    ADD COLUMN include_descendants BOOLEAN NOT NULL DEFAULT TRUE;
