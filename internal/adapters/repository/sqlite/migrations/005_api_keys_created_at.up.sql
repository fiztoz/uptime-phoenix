ALTER TABLE api_keys ADD COLUMN created_at TEXT NOT NULL DEFAULT (datetime('now'));
