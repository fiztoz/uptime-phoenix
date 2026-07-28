-- Persist the authenticator state required to validate subsequent passkey
-- assertions. NULL flags identify credentials registered before this migration;
-- their first successfully verified assertion backfills the signed flag byte.

ALTER TABLE webauthn_credentials ADD COLUMN flags INTEGER;
ALTER TABLE webauthn_credentials ADD COLUMN clone_warning INTEGER NOT NULL DEFAULT 0;
ALTER TABLE webauthn_credentials ADD COLUMN attachment TEXT NOT NULL DEFAULT '';
