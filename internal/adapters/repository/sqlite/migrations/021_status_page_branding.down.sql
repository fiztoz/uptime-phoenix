-- SQLite cannot DROP COLUMN on older versions used in tests without table rebuild.
-- Leave columns inert on down for edge SQLite; MariaDB has a full reverse migration.
SELECT 1;
