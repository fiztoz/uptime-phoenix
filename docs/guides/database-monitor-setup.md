# Database monitor — setup guide

Phoenix’s **Database** monitor checks that a database is **reachable and authenticating**, using a dedicated connection string and engine.

It does **not** run free-form SQL you type in the UI. That avoids injection and accidental writes. Instead you choose a **health check preset**:

| Preset | What runs |
|--------|-----------|
| **Connect + protocol ping only** (`ping`) | Driver/protocol ping after connect |
| **Also run fixed SELECT 1 / PING** (`select_1`) | Same connect, then a **fixed** statement Phoenix chooses (`SELECT 1` for SQL; `PING` for Redis; Mongo ping) |

Supported engines: **PostgreSQL**, **MySQL**, **MariaDB**, **SQL Server (MSSQL)**, **MongoDB**, **Redis**.

---

## Form fields

| Field | Config key | Notes |
|-------|------------|--------|
| Engine | `engine` | Required |
| Connection string / DSN | `connection_string` | Required; older configs may use `dsn` |
| Health check | `health_check` | `ping` (default) or `select_1` |
| Timeout (s) | `timeout` | Default `10` |

---

## Connection string examples

Use a **least-privilege monitor user** (scripts below). Replace hosts, passwords, and DB names.

### PostgreSQL

```text
postgres://phoenix_monitor:CHANGE_ME@db.example.com:5432/appdb?sslmode=require
```

Local / no TLS (lab only):

```text
postgres://phoenix_monitor:CHANGE_ME@127.0.0.1:5432/appdb?sslmode=disable
```

### MySQL / MariaDB

Phoenix uses the Go MySQL driver DSN form:

```text
phoenix_monitor:CHANGE_ME@tcp(db.example.com:3306)/appdb?parseTime=true&tls=true
```

Or classic URL-style (driver-dependent; prefer the form above if unsure):

```text
user:CHANGE_ME@tcp(127.0.0.1:3306)/appdb?parseTime=true&timeout=5s
```

### SQL Server (MSSQL)

URL style:

```text
sqlserver://phoenix_monitor:CHANGE_ME@sql.example.com:1433?database=appdb
```

ADO-style:

```text
server=sql.example.com,1433;user id=phoenix_monitor;password=CHANGE_ME;database=appdb;encrypt=true;TrustServerCertificate=false
```

### MongoDB

```text
mongodb://phoenix_monitor:CHANGE_ME@mongo.example.com:27017/appdb?authSource=admin
```

### Redis

```text
redis://:CHANGE_ME@redis.example.com:6379/0
```

TLS:

```text
rediss://:CHANGE_ME@redis.example.com:6380/0
```

Plain host:port (no password):

```text
redis.example.com:6379
```

---

## Create a least-privilege monitor user

**Do not** use your app’s admin or superuser credentials in Phoenix.

Scripts (repo + served with the web UI):

| Engine | Repo path | Served URL |
|--------|-----------|------------|
| PostgreSQL | `docs/scripts/database-monitor/create-user-postgres.sql` | `/docs/database-monitor/create-user-postgres.sql` |
| MySQL / MariaDB | `docs/scripts/database-monitor/create-user-mysql.sql` | `/docs/database-monitor/create-user-mysql.sql` |
| SQL Server | `docs/scripts/database-monitor/create-user-mssql.sql` | `/docs/database-monitor/create-user-mssql.sql` |
| MongoDB | `docs/scripts/database-monitor/create-user-mongodb.js` | `/docs/database-monitor/create-user-mongodb.js` |
| Redis | `docs/scripts/database-monitor/create-user-redis.md` | `/docs/database-monitor/create-user-redis.md` |

Also available from the Phoenix UI: **Create/Edit monitor → Database → View setup guide**.

### Quick start (examples)

**PostgreSQL** (as a superuser):

```bash
psql -h db.example.com -U postgres -d appdb -f docs/scripts/database-monitor/create-user-postgres.sql
```

Edit the script first: set password and database name.

**MySQL / MariaDB:**

```bash
mysql -h db.example.com -u root -p appdb < docs/scripts/database-monitor/create-user-mysql.sql
```

**SQL Server** (sqlcmd):

```bash
sqlcmd -S sql.example.com -U sa -i docs/scripts/database-monitor/create-user-mssql.sql
```

**MongoDB:**

```bash
mongosh "mongodb://admin:…@mongo.example.com:27017/admin" docs/scripts/database-monitor/create-user-mongodb.js
```

Then paste the resulting DSN into Phoenix with **Engine** set correctly and preferably **Health check = Also run fixed SELECT 1 / PING**.

---

## Recommended setup checklist

1. Run the create-user script for your engine (or equivalent grants).
2. From a host that can reach the DB the same way Phoenix will, test the DSN (e.g. `psql`, `mysql`, `mongosh`, `redis-cli`).
3. In Phoenix: **Monitors → Create → Database**.
4. Set **Engine**, **Connection string**, **Health check**, **Timeout**.
5. Save; wait one interval. Status should go **UP**.
6. Optionally break auth or block the port and confirm **DOWN** + a useful message.

---

## Network & security

- Phoenix must reach the DB from **its** network (pod, VM, or host).
- Prefer TLS (`sslmode=require`, `tls=true`, `encrypt=true`, `rediss://`, Mongo `tls=true`).
- Store the DSN only in the monitor config (encrypted at rest by your DB volume / secrets practice). Never put passwords in the monitor **name**.
- Prefer **private network** access; avoid exposing DB ports publicly only for monitoring.
- Free-form queries are intentionally **not** supported — use HTTP/push monitors for app-level “is my query path healthy?” if you need business logic checks.

---

## Common failures

| Symptom | Likely cause |
|---------|----------------|
| Dial / timeout | Firewall, wrong host/port, Phoenix in another network namespace |
| Auth failed | Wrong password, user host restriction (MySQL `'user'@'%'`), `authSource` (Mongo) |
| TLS / certificate errors | Missing `sslmode` / `tls` / `encrypt`, or untrusted CA |
| SELECT 1 fails but ping works | User lacks CONNECT or basic SELECT on the target DB (rare with scripts above) |
| Works from laptop, fails in Phoenix | Different egress IP / security group; allow Phoenix’s source |

---

## Related monitors

| Need | Prefer |
|------|--------|
| Only “port open” | **TCP** |
| App HTTP health that already hits the DB | **HTTP(s)** |
| App-owned liveness without DB credentials in Phoenix | **Push** |
| Redis as a cache broker vs DB | Still use **Database** + engine Redis, or TCP if you only care about the port |
