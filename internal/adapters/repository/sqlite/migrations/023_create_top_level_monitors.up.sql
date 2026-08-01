-- Optional non-admin placement power: create monitors with no group (top-level).
ALTER TABLE users ADD COLUMN can_create_top_level_monitors INTEGER NOT NULL DEFAULT 0;
