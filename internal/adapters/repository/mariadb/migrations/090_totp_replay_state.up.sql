-- TOTP replay protection (issue #123).
--
-- totp_generation identifies the stored TOTP credential. Every secret
-- replacement or removal (setup, re-setup, disable) increments it in the same
-- statement that writes the secret, and resets totp_last_step to 0.
--
-- totp_last_step is the latest TOTP time step (Unix seconds / 30) already
-- consumed by a successful login or setup confirmation. A code is accepted
-- only by a single conditional UPDATE that requires the expected generation
-- and a strictly later step, so concurrent requests on any number of hub
-- replicas elect at most one winner and a code is never usable twice.
--
-- Existing rows start at generation 0 / step 0: the first code after upgrade
-- is accepted once. Neither column is written by ordinary user updates,
-- exposed through the API or included in configuration backups.
-- ADD COLUMN IF NOT EXISTS keeps the script idempotent (068 tail-heal idiom).
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS totp_generation BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS totp_last_step BIGINT NOT NULL DEFAULT 0;
