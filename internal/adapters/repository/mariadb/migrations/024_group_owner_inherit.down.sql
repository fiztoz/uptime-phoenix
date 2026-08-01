ALTER TABLE monitors
    DROP COLUMN IF EXISTS inherit_group_owner;

ALTER TABLE monitor_groups
    DROP COLUMN IF EXISTS owner;
