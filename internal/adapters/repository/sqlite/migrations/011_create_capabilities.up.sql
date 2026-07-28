-- 011_create_capabilities.up.sql — SQLite: creation capabilities + shallow group grants
-- Created_at: 2026-07-17
-- Updated_at: 2026-07-17
--
-- Two independent pieces, both extending the model 008 established.
--
-- 1. users.can_create_monitors / users.can_create_groups — two more capability
--    flags in the same family as can_manage_notifications / can_manage_maintenance.
--    They relax the rule 008 wrote down ("monitors and groups stay admin-only to
--    mutate"): a non-admin holding one may CREATE that resource type, and may
--    then edit and delete what they themselves created — never what someone else
--    created. Ownership is decided by monitors.user_id / monitor_groups.user_id,
--    which have always been set to the creating user; nothing new is stored for
--    it here. AccessService.CanEditMonitor / CanEditGroup are the only readers.
--
--    Default 0, so this migration grants nobody anything: every existing
--    non-admin stays exactly as read-only as they were before it ran. As in 008,
--    admins need no backfill — IsAdmin short-circuits every capability check in
--    the service.
--
-- 2. user_permissions.include_descendants — makes the recursion of a GROUP grant
--    a per-grant choice instead of a hard-coded always.
--
--    Default 1 REPLICATES THE PRE-MIGRATION BEHAVIOR: 008-era group grants were
--    unconditionally recursive, so every existing row must keep cascading or
--    users silently lose visibility the morning after an upgrade. Never change
--    this default to 0.
--
--      include_descendants = 1 (deep, the default): the group, every descendant
--        subgroup, and every monitor filed under any of them.
--      include_descendants = 0 (shallow): the group and the monitors filed
--        DIRECTLY in it. Subgroups and their contents stay invisible.
--
--    The column is meaningless for a monitor grant (monitor_id set, group_id
--    NULL) — a monitor has no descendants. It is left NOT NULL DEFAULT 1 rather
--    than CHECK-constrained against monitor grants, because the service reads it
--    only on the group branch and a stray 1 on a monitor row decides nothing.
--
-- SQLite cannot add a column to a table with a non-constant DEFAULT in one step,
-- but a literal 1 is constant, so a plain ALTER is fine here — no table rebuild.

ALTER TABLE users ADD COLUMN can_create_monitors INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN can_create_groups INTEGER NOT NULL DEFAULT 0;

ALTER TABLE user_permissions ADD COLUMN include_descendants INTEGER NOT NULL DEFAULT 1;
