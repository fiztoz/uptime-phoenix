-- Phoenix Database monitor — least-privilege SQL Server user
-- Edit password and database name, then run as sysadmin (sqlcmd / SSMS):
--   sqlcmd -S HOST -U sa -i create-user-mssql.sql
--
-- Grants: CONNECT + ability to run SELECT 1 in the target database.
-- Does NOT grant data writer or ddl_admin.

-- === EDIT: login password ===
IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'phoenix_monitor')
BEGIN
    CREATE LOGIN phoenix_monitor WITH PASSWORD = N'CHANGE_ME_STRONG_Password1', CHECK_POLICY = ON;
END
ELSE
BEGIN
    ALTER LOGIN phoenix_monitor WITH PASSWORD = N'CHANGE_ME_STRONG_Password1';
END
GO

-- === EDIT: database name (replace appdb) ===
USE appdb;
GO

IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'phoenix_monitor')
BEGIN
    CREATE USER phoenix_monitor FOR LOGIN phoenix_monitor;
END
GO

-- Minimal rights for connect + SELECT 1
ALTER ROLE db_datareader ADD MEMBER phoenix_monitor;
-- If you prefer not to grant table reads, use only CONNECT (SELECT 1 still works for most editions):
-- GRANT CONNECT TO phoenix_monitor;
GO

-- Verify:
-- sqlcmd -S HOST -U phoenix_monitor -P '…' -d appdb -Q "SELECT 1"

-- Phoenix connection string examples:
-- sqlserver://phoenix_monitor:CHANGE_ME_STRONG_Password1@HOST:1433?database=appdb
-- server=HOST,1433;user id=phoenix_monitor;password=CHANGE_ME_STRONG_Password1;database=appdb;encrypt=true
