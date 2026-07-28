-- 011_create_capabilities.down.sql — MariaDB: revert creation capabilities + shallow group grants
-- Created_at: 2026-07-17
-- Updated_at: 2026-07-17
--
-- InnoDB drops columns in place, so unlike the SQLite twin no table rebuild is
-- needed here.
--
-- Reverting is lossy in one direction, and it is the safe one: a shallow grant
-- (include_descendants = FALSE) becomes recursive again, because 008-era code
-- cannot express "this folder but not its subfolders". That WIDENS visibility
-- back to what the older schema would always have given. Re-check any user who
-- held a shallow grant after a downgrade — the narrowing does not survive it.

ALTER TABLE user_permissions
    DROP COLUMN include_descendants;

ALTER TABLE users
    DROP COLUMN can_create_monitors,
    DROP COLUMN can_create_groups;
