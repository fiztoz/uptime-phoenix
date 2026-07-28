-- SQLite cannot DROP COLUMN on older versions without a table rebuild.
-- Phoenix targets modernc SQLite which supports DROP COLUMN (3.35+).

ALTER TABLE maintenance_windows DROP COLUMN timezone;
ALTER TABLE monitors DROP COLUMN cert_expiry_notify;
