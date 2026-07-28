ALTER TABLE users ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0;
-- Backfill: before RBAC every existing user was effectively an admin (no access
-- control existed). Promote all pre-existing users so upgraded installs keep at
-- least one admin and are not locked out of user management. On a fresh install
-- the users table is empty here, so this is a harmless no-op.
UPDATE users SET is_admin = 1;
