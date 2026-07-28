-- 007_monitor_groups.down.sql — MariaDB: revert monitor groups
-- Created_at: 2026-07-12
-- Updated_at: 2026-07-12
--
-- Drops monitor_groups and restores monitors.parent_id. No data migration —
-- dev data only; group_id assignments are not carried back to parent_id.
-- The restored FK is given the explicit name fk_monitors_parent so a
-- subsequent re-application of 007_monitor_groups.up.sql can find and drop
-- it deterministically (see the comment there).

ALTER TABLE monitors
    DROP FOREIGN KEY IF EXISTS fk_monitors_group,
    DROP COLUMN group_id,
    ADD COLUMN parent_id BIGINT DEFAULT NULL,
    ADD CONSTRAINT fk_monitors_parent FOREIGN KEY (parent_id) REFERENCES monitors(id) ON DELETE SET NULL;

DROP TABLE IF EXISTS monitor_groups;
