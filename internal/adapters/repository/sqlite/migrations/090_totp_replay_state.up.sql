-- TOTP replay protection (issue #123). See the MariaDB migration of the same
-- number for the full rationale.
ALTER TABLE users ADD COLUMN totp_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN totp_last_step INTEGER NOT NULL DEFAULT 0;
