-- Per-user visibility for operator-registered extension pages.
-- Preserve the pre-031 behavior for existing non-admins; users created after
-- this migration default to no extension access until an admin grants it.
ALTER TABLE users
    ADD COLUMN can_view_extensions BOOLEAN NOT NULL DEFAULT FALSE
        AFTER can_edit_group_metadata;

UPDATE users
SET can_view_extensions = TRUE
WHERE is_admin = FALSE;
