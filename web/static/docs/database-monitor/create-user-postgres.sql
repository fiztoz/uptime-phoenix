-- Phoenix Database monitor — least-privilege PostgreSQL user
-- Edit password and database name, then run as a superuser:
--   psql -h HOST -U postgres -d appdb -f create-user-postgres.sql
--
-- Grants: CONNECT + USAGE on schema public + SELECT on existing tables.
-- Enough for health_check=ping and health_check=select_1 (SELECT 1).
-- Does NOT grant INSERT/UPDATE/DELETE/DDL.

\set ON_ERROR_STOP on

-- === EDIT THESE ===
-- Password and target database:
-- (If you prefer variables, set them with -v before running.)

DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'phoenix_monitor') THEN
    CREATE ROLE phoenix_monitor LOGIN PASSWORD 'CHANGE_ME_STRONG_PASSWORD';
  ELSE
    ALTER ROLE phoenix_monitor WITH LOGIN PASSWORD 'CHANGE_ME_STRONG_PASSWORD';
  END IF;
END
$$;

-- Replace appdb with your application database name:
GRANT CONNECT ON DATABASE appdb TO phoenix_monitor;

\c appdb

GRANT USAGE ON SCHEMA public TO phoenix_monitor;
-- Optional: allow SELECT on app tables (not required for SELECT 1, useful for future checks)
-- GRANT SELECT ON ALL TABLES IN SCHEMA public TO phoenix_monitor;
-- ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO phoenix_monitor;

-- Verify: should return 1
-- SET ROLE phoenix_monitor; SELECT 1;

-- Phoenix connection string example:
-- postgres://phoenix_monitor:CHANGE_ME_STRONG_PASSWORD@HOST:5432/appdb?sslmode=require
