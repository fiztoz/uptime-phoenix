-- Phoenix Database monitor — least-privilege MySQL / MariaDB user
-- Edit password, host pattern, and database name, then run as admin:
--   mysql -h HOST -u root -p < create-user-mysql.sql
--
-- Grants: USAGE + SELECT only on the target schema (SELECT 1 works without table grants).
-- Does NOT grant write or DDL privileges.

-- === EDIT THESE ===
CREATE USER IF NOT EXISTS 'phoenix_monitor'@'%' IDENTIFIED BY 'CHANGE_ME_STRONG_PASSWORD';
-- Prefer tighter host if Phoenix has a fixed egress IP, e.g. 'phoenix_monitor'@'10.0.0.%'

-- Target database (replace appdb):
GRANT USAGE ON *.* TO 'phoenix_monitor'@'%';
GRANT SELECT ON `appdb`.* TO 'phoenix_monitor'@'%';
-- For SELECT 1 only, USAGE is enough; SELECT on schema is optional documentation of intent.

FLUSH PRIVILEGES;

-- Verify:
-- mysql -h HOST -u phoenix_monitor -p -e 'SELECT 1'

-- Phoenix connection string example (Go MySQL DSN):
-- phoenix_monitor:CHANGE_ME_STRONG_PASSWORD@tcp(HOST:3306)/appdb?parseTime=true&tls=true
