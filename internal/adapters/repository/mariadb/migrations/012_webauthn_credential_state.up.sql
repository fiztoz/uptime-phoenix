-- Persist the authenticator state required to validate subsequent passkey
-- assertions. NULL flags identify credentials registered before this migration;
-- their first successfully verified assertion backfills the signed flag byte.

ALTER TABLE webauthn_credentials
    ADD COLUMN flags TINYINT UNSIGNED NULL,
    ADD COLUMN clone_warning BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN attachment VARCHAR(64) NOT NULL DEFAULT '';
