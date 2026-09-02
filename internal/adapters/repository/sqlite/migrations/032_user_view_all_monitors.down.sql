-- SQLite deployments may retain the additive column on downgrade. Older
-- Phoenix binaries ignore it, and avoiding a users-table rebuild is safer.
SELECT 1;
