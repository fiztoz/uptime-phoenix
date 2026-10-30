-- Revert TOTP replay protection. An older binary never reads or writes these
-- columns; dropping them returns to verification without replay refusal.
ALTER TABLE users
    DROP COLUMN IF EXISTS totp_last_step,
    DROP COLUMN IF EXISTS totp_generation;
