-- Install-wide read-only visibility for non-admins. Default off: this is a new
-- privilege, not a preservation of old behavior. Admins already see everything
-- via is_admin; the raw flag stays false for them.
ALTER TABLE users ADD COLUMN can_view_all_monitors INTEGER NOT NULL DEFAULT 0;
