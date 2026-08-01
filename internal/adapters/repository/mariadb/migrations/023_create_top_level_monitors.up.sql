-- Optional non-admin placement power: create monitors with no group (top-level).
-- Requires can_create_monitors for the create route; this flag only widens where.
ALTER TABLE users
    ADD COLUMN can_create_top_level_monitors BOOLEAN NOT NULL DEFAULT FALSE
        AFTER can_create_monitors;
