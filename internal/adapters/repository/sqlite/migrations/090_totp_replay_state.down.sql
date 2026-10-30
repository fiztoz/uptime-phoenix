-- Revert TOTP replay protection. SQLite learned DROP COLUMN in 3.35; the
-- modernc.org/sqlite runtime used here is well past that.
ALTER TABLE users DROP COLUMN totp_last_step;
ALTER TABLE users DROP COLUMN totp_generation;
