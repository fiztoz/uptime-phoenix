-- SQLite's migration runner keeps down migrations reversible at the logical
-- level; older SQLite versions cannot DROP COLUMN safely in every deployment.
SELECT 1;
