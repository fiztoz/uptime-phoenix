-- Non-admin may edit group metadata (not name/parent/delete) on groups they can view.
ALTER TABLE users
    ADD COLUMN can_edit_group_metadata BOOLEAN NOT NULL DEFAULT FALSE
        AFTER can_create_groups;
