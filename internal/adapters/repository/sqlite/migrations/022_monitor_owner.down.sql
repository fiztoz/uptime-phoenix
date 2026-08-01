-- Keep the additive column on down for compatibility with older SQLite builds
-- that cannot drop columns. MariaDB has a full reverse migration.
SELECT 1;
