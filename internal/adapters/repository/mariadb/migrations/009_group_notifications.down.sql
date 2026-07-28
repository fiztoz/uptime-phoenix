-- 009_group_notifications.down.sql — MariaDB: revert group notifications
-- Created_at: 2026-07-13
-- Updated_at: 2026-07-13
--
-- group_notifications is dropped FIRST: its FK to monitor_groups(id) would
-- otherwise have to be dropped separately before the table it points at can be
-- altered. Dropping the child table takes both FKs with it.
--
-- Unlike 007's down, nothing here has to guess at an auto-generated constraint
-- name: the up migration names both FKs, and they die with the table.
--
-- IF EXISTS on the DROP COLUMN so an up -> down -> down sequence is a no-op
-- rather than an error.

DROP TABLE IF EXISTS group_notifications;

ALTER TABLE monitor_groups DROP COLUMN IF EXISTS last_status;
