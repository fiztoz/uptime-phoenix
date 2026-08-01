-- Group contact + monitor option to inherit it (and ancestor contacts).
ALTER TABLE monitor_groups
    ADD COLUMN owner VARCHAR(255) NOT NULL DEFAULT '' AFTER description;

ALTER TABLE monitors
    ADD COLUMN inherit_group_owner BOOLEAN NOT NULL DEFAULT FALSE AFTER owner;
