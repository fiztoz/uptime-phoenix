# Database monitor — setup guide

Uptime Phoenix’s **Database** monitor checks that a database is **reachable and authenticating**, using a dedicated connection string and engine.

It does **not** run free-form SQL you type in the UI. That avoids injection and accidental writes. Instead you choose a **health check preset**:

| Preset                                          | What runs                                                                                                          |
| ----------------------------------------------- | ------------------------------------------------------------------------------------------------------------------ |
| **Connect + protocol ping only** (`ping`)       | Driver/protocol ping after connect                                                                                 |
| **Also run fixed SELECT 1 / PING** (`select_1`) | Same connect, then a **fixed** statement Uptime Phoenix chooses (`SELECT 1` for SQL; `PING` for Redis; Mongo ping) |

Supported engines: **PostgreSQL**, **MySQL**, **MariaDB**, **SQL Server (MSSQL)**, **MongoDB**, **Redis**.

---

## Form fields

| Field                      | Config key               | Notes                                                                                                                              |
| -------------------------- | ------------------------ | ---------------------------------------------------------------------------------------------------------------------------------- |
| Engine                     | `engine`                 | Required                                                                                                                           |
| Connection string / DSN    | `connection_string`      | Required; older configs may use `dsn`                                                                                              |
| Health check               | `health_check`           | `ping` (default) or `select_1` — presets only, never free-form SQL                                                                 |
| Timeout (s)                | `timeout`                | Default `10`                                                                                                                       |
| Check session pool         | `check_session_pool`     | Optional; default `false`                                                                                                          |
| Session pool threshold (%) | `session_pool_threshold` | Default `80` (1–100); used when session-pool check is on                                                                           |
| Check storage              | `check_storage`          | Optional; default `false`                                                                                                          |
| Storage scope              | `storage_scope`          | `database` (default) or `instance`; used when storage check is on                                                                  |
| Storage threshold (%)      | `storage_threshold`      | Default `80` (1–100); used when storage check is on                                                                                |
| Max size (GiB)             | `storage_max_gb`         | Optional capacity in GiB (1024³ bytes). Required at check time for engines that cannot report a total (typical PostgreSQL / MySQL). Match this to the chosen storage scope. |

---

## Capacity alerts (optional)

After a successful connect (and the chosen health preset), Uptime Phoenix can also measure **session-pool usage** and **storage usage**. Both are **off by default**.

These checks still **never run operator SQL**. Each engine uses a **fixed** server query, `INFO`, or `dbStats` command. You cannot type a query in the UI.

If usage is **at or above** the threshold, the database heartbeat remains
**UP / Operational**. The capacity condition becomes `warning` only after
**two consecutive** over-threshold samples (the first sample is unconfirmed
and is not shown as a chip). Uptime Phoenix then shows an amber condition
chip and notifies. Example: `session pool 92/100 (92.0%) exceeds threshold 80%`.

Capacity does not count as Insights downtime, lower uptime/SLA, trip a folder
to DOWN, change a status badge, or degrade a public status page. It also does
not add a fifth heartbeat `WARNING` status; yellow heartbeat state remains
PENDING for the retry window. See
[`database-capacity-presentation.md`](./database-capacity-presentation.md).

A missing privilege or unknown capacity becomes condition `error` after
two consecutive failed samples, not a silent skip and not fake downtime.
On an active monitor it appears in **Needs attention** and uses the same
condition-notification path. Paused or maintenance monitors keep their
last row for context but do not become attention items merely because
that row goes stale.

Conditions recover only after two consecutive normal samples. A warning does
not clear until utilization is at least 5 percentage points below its threshold
(for an 80% threshold, below 75%). Capacity queries run on every primary check.
If no fresh sample arrives within three monitor intervals (minimum three
minutes), the UI derives `stale`.

### Config keys

| Key                      | Type    | Default | Meaning                                                                                                                                                                                                                                                                         |
| ------------------------ | ------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `check_session_pool`     | boolean | `false` | Enable session-pool check                                                                                                                                                                                                                                                       |
| `session_pool_threshold` | number  | `80`    | Percent 1–100; condition becomes `warning` at or above                                                                                                                                                                                                                          |
| `check_storage`          | boolean | `false` | Enable storage check (fixed SQL / INFO / dbStats)                                                                                                                                                                                                                               |
| `storage_scope`          | string  | `database` | `database` = connected database only. `instance` = all non-template (PostgreSQL) or visible (MySQL) databases on the instance. Ignored by Redis, MongoDB, and SQL Server. MariaDB `DISKS` stays volume-level; the MySQL-style fallback honors this key. |
| `storage_threshold`      | number  | `80`    | Percent 1–100; condition becomes `warning` at or above                                                                                                                                                                                                                          |
| `storage_max_gb`         | number  | unset   | Optional capacity in GiB (1024³ bytes). Required at check time for engines that cannot report a total (typical PostgreSQL / MySQL). Compare against the same allocation as `storage_scope` — one database, or the instance data allocation. Redis uses `maxmemory` when set; Mongo may use `fsTotalSize`; MariaDB may use `information_schema.DISKS`; MSSQL may use database file size. |

### What each engine measures

| Engine     | Sessions                                                               | Storage                                                                                                               |
| ---------- | ---------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------- |
| PostgreSQL | `SUM(numbackends)` / `max_connections` (usually works)                 | Default: `pg_database_size(current_database())` vs `storage_max_gb` (CONNECT is enough). `storage_scope=instance`: sum `pg_database_size` for every non-template database (needs CONNECT on each, or `GRANT pg_read_all_stats`). **Not** host disk — excludes WAL, logs, temp, backups, and free space. |
| MySQL      | `Threads_connected` / `max_connections`                                | Default: `information_schema.tables` for `DATABASE()` vs `storage_max_gb`. `storage_scope=instance`: same sum across visible schemas (only tables the user can see). |
| MariaDB    | same as MySQL                                                          | `DISKS` if plugin + FILE privilege (values are KiB; Phoenix converts to bytes; volume-level, ignores `storage_scope`), else same as MySQL + `storage_max_gb` |
| SQL Server | `dm_exec_sessions` / `@@MAX_CONNECTIONS` — needs **VIEW SERVER STATE** | `sys.database_files` used vs allocated (or `storage_max_gb`)                                                          |
| MongoDB    | `serverStatus.connections` — needs **clusterMonitor**                  | `dbStats` `fsUsedSize`/`fsTotalSize` or `storageSize` vs `storage_max_gb`                                             |
| Redis      | `INFO clients` — needs **+info**                                       | `INFO memory` `used_memory` vs `maxmemory` (**memory**, not disk)                                                     |

**Do not** use a superuser just to turn these on. Add the documented **optional grants** to the dedicated `phoenix_monitor` user in the create-user scripts, then enable the checkboxes.

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

Uptime Phoenix uses the Go MySQL driver DSN form:

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

**Do not** use your app’s admin or superuser credentials in Uptime Phoenix.

Scripts (repo + served with the web UI):

| Engine          | Repo path                                                | Served URL                                        |
| --------------- | -------------------------------------------------------- | ------------------------------------------------- |
| PostgreSQL      | `docs/scripts/database-monitor/create-user-postgres.sql` | `/docs/database-monitor/create-user-postgres.sql` |
| MySQL / MariaDB | `docs/scripts/database-monitor/create-user-mysql.sql`    | `/docs/database-monitor/create-user-mysql.sql`    |
| SQL Server      | `docs/scripts/database-monitor/create-user-mssql.sql`    | `/docs/database-monitor/create-user-mssql.sql`    |
| MongoDB         | `docs/scripts/database-monitor/create-user-mongodb.js`   | `/docs/database-monitor/create-user-mongodb.js`   |
| Redis           | `docs/scripts/database-monitor/create-user-redis.md`     | `/docs/database-monitor/create-user-redis.md`     |

Also available from the Uptime Phoenix UI: **Create/Edit monitor → Database → View setup guide**.

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

Then paste the resulting DSN into Uptime Phoenix with **Engine** set correctly and preferably **Health check = Also run fixed SELECT 1 / PING**.

---

## Recommended setup checklist

1. Run the create-user script for your engine (or equivalent grants).
2. From a host that can reach the DB the same way Uptime Phoenix will, test the DSN (e.g. `psql`, `mysql`, `mongosh`, `redis-cli`).
3. In Uptime Phoenix: **Monitors → Create → Database**.
4. Set **Engine**, **Connection string**, **Health check**, **Timeout**.
5. Save; wait one interval. Status should go **UP**.
6. Optionally break auth or block the port and confirm **DOWN** + a useful message.
7. If you enable **capacity alerts**: apply the optional grants in the create-user script (not a superuser), set **Max size (GiB)** when the engine cannot report a total (typical PostgreSQL / MySQL), choose **Storage scope** (`This database only` or `All databases on the instance`) so the max matches what you measure, then turn on the checkboxes. Capacity queries run on every primary check — keep the monitor interval at 30s or slower on busy engines. A warning chip and notification appear only after two consecutive samples.

---

## Network & security

- Uptime Phoenix must reach the DB from **its** network (pod, VM, or host).
- Prefer TLS (`sslmode=require`, `tls=true`, `encrypt=true`, `rediss://`, Mongo `tls=true`).
- Store the DSN only in the monitor config (encrypted at rest by your DB volume / secrets practice). Never put passwords in the monitor **name**.
- Prefer **private network** access; avoid exposing DB ports publicly only for monitoring.
- Free-form queries are intentionally **not** supported — use HTTP/push monitors for app-level “is my query path healthy?” if you need business logic checks.

---

## Common failures

| Symptom                                            | Likely cause                                                                                                                                                                  |
| -------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Dial / timeout                                     | Firewall, wrong host/port, Uptime Phoenix in another network namespace                                                                                                        |
| Auth failed                                        | Wrong password, user host restriction (MySQL `'user'@'%'`), `authSource` (Mongo)                                                                                              |
| TLS / certificate errors                           | Missing `sslmode` / `tls` / `encrypt`, or untrusted CA                                                                                                                        |
| SELECT 1 fails but ping works                      | User lacks CONNECT or basic SELECT on the target DB (rare with scripts above)                                                                                                 |
| Works from laptop, fails in Uptime Phoenix         | Different egress IP / security group; allow Uptime Phoenix’s source                                                                                                           |
| Capacity chip says Check error / permission denied | User lacks the optional grant (`VIEW SERVER STATE`, `clusterMonitor`, `+info`, MariaDB `FILE` for `DISKS`, …). Availability stays UP, but the auxiliary check is not skipped. |
| Storage condition says capacity unknown            | Set `storage_max_gb` for PostgreSQL/MySQL (and MariaDB when `DISKS` is unavailable)                                                                                           |
| Capacity chip says Stale                           | No fresh auxiliary sample arrived within three monitor intervals (minimum three minutes); inspect scheduler/worker health and the latest primary check                        |

---

## Related monitors

| Need                                                        | Prefer                                                                        |
| ----------------------------------------------------------- | ----------------------------------------------------------------------------- |
| Only “port open”                                            | **TCP**                                                                       |
| App HTTP health that already hits the DB                    | **HTTP(s)**                                                                   |
| App-owned liveness without DB credentials in Uptime Phoenix | **Push**                                                                      |
| Redis as a cache broker vs DB                               | Still use **Database** + engine Redis, or TCP if you only care about the port |
