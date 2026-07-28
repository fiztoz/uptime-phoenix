# Uptime Kuma → Phoenix importer

Convert a **stopped** Uptime Kuma snapshot into a Phoenix `BackupDocument`
JSON file, then import that file through the existing admin backup API.

Supported sources:

| Source | Kuma versions | How |
|---|---|---|
| **SQLite file** | v1 and v2 file-backed installs | `--input /path/to/kuma.db` |
| **MariaDB / MySQL** | v2 external DB (same redbean schema as SQLite) | `--engine mariadb --dsn '…'` |

The importer is a **read-only offline converter**. It never writes the Kuma
database, never writes a Phoenix database, and never calls a live Phoenix API.

## Secret warning

The output JSON is a **credential bundle**. It can contain:

- notification provider tokens and webhook URLs
- proxy passwords
- push tokens
- status-page password hashes

Treat it the same way as a Phoenix backup export (see `docs/RUNBOOK.md` §1):

- write it only to a secure path (the CLI uses mode `0600`)
- never commit it, never paste it into tickets or chat
- delete it after a successful import according to your secret-handling policy
- the optional `--report` file is safe (counts and skip reasons only)
- **never log or paste `--dsn` values** — they embed the database password

## Prerequisites

### SQLite source

1. **Stop Uptime Kuma** (or freeze I/O) so the SQLite file is consistent.
2. **Snapshot the database**:
   - Preferred: stop the process, then copy `kuma.db` (and any `-wal`/`-shm` if
     you are not fully stopped — better to stop first).
   - Docker example:
     ```bash
     docker stop uptime-kuma
     docker cp uptime-kuma:/app/data/kuma.db ./kuma.db
     ```
3. Keep a full backup of Kuma until you have verified the Phoenix import.

### MariaDB / MySQL source (Kuma v2)

1. Prefer a **read-only database user** with `SELECT` only on the Kuma schema.
2. **Stop or quiesce writes** to Kuma (stop the app, or take a consistent
   snapshot / dump) so you are not converting a half-written check cycle.
3. Optional consistency path: `mysqldump --single-transaction` into a throwaway
   database and point `--dsn` at that clone.
4. Keep the dump until Phoenix import is verified.

DSN format (`go-sql-driver/mysql`):

```text
user:password@tcp(host:3306)/kuma?parseTime=true&charset=utf8mb4
```

`parseTime=true` is forced on when missing. Prefer loading the password from the
environment rather than a shell history entry:

```bash
export KUMA_DSN="kuma_ro:${KUMA_DB_PASSWORD}@tcp(127.0.0.1:3306)/kuma?parseTime=true"
```

## CLI

### SQLite

```bash
go run ./cmd/kuma-import \
  --input /path/to/kuma.db \
  --output /secure/path/phoenix-backup.json

# Optional safe summary (no secrets):
go run ./cmd/kuma-import \
  --input /path/to/kuma.db \
  --output /secure/path/phoenix-backup.json \
  --report /secure/path/kuma-import-report.json

# Fail if anything supported-looking was skipped:
go run ./cmd/kuma-import \
  --input /path/to/kuma.db \
  --output /secure/path/phoenix-backup.json \
  --strict

# Overwrite an existing output file:
go run ./cmd/kuma-import \
  --input /path/to/kuma.db \
  --output /secure/path/phoenix-backup.json \
  --force
```

### MariaDB (Kuma v2)

```bash
go run ./cmd/kuma-import \
  --engine mariadb \
  --dsn "$KUMA_DSN" \
  --output /secure/path/phoenix-backup.json \
  --report /secure/path/kuma-import-report.json

# --engine can be omitted when only --dsn is provided (inferred as mariadb).
# mysql is accepted as an alias of mariadb (same driver and schema).
```

Flags:

| Flag | Required | Description |
|---|---|---|
| `--engine` | no | `sqlite` (default) or `mariadb` (`mysql` alias). Inferred from flags when omitted. |
| `--input` | for sqlite | Path to Kuma SQLite file (opened `mode=ro`) |
| `--dsn` | for mariadb | MariaDB/MySQL DSN (session set `READ ONLY` when permitted) |
| `--output` | yes | Phoenix backup JSON path (created mode `0600`) |
| `--report` | no | Safe summary JSON (counts + skip reasons + engine label; no secrets) |
| `--force` | no | Allow overwriting `--output` |
| `--strict` | no | Nonzero exit when any entity is skipped |

Build a permanent binary:

```bash
CGO_ENABLED=0 go build -o kuma-import ./cmd/kuma-import
```

## Support matrix

### Monitors (Kuma type → Phoenix type)

| Kuma type | Phoenix type | Notes |
|---|---|---|
| `http` | `http` | URL, method, headers, body, redirects |
| `keyword` | `http` | keyword → config.keyword |
| `json-query` | `http` | json_path → config.json_query |
| `port` | `tcp` | hostname + port |
| `ping` | `ping` | hostname |
| `dns` | `dns` | resolve type/server when present |
| `websocket` / `websocket-keyword` | `websocket` | URL required |
| `push` | `push` | push_token required |
| `docker` | `docker` | container name/id |
| `mqtt` | `mqtt` | broker/topic/auth |
| `rabbitmq` | `rabbitmq` | AMQP URL, or hostname + port |
| `grpc` / `grpc-keyword` | `grpc` | URL / service name |
| `snmp` | `snmp` | hostname + oid when columns present |
| `postgres` / `postgresql` | `database` | engine=postgres |
| `mysql` / `mariadb` | `database` | matching engine |
| `mongodb` / `mongo` | `database` | engine=mongodb |
| `redis` | `database` | engine=redis |
| `group` | **folder** | becomes `monitor_groups`, not a monitor |
| `systemd` / `system-service` | — | **skipped** (deferred until Phoenix agent) |
| `gamedig` / `steam` | — | **skipped** (excluded from Phoenix) |
| `tailscale` | — | **skipped** (excluded from Phoenix) |
| `radius` | — | **skipped** (excluded from Phoenix) |
| `kafka` / `kafka-producer` | — | **skipped** (excluded) |
| `sqlserver` | `database` | engine=mssql |
| `real-browser` | — | **skipped** (no equivalent) |
| other | — | **skipped** with reason |

Also converted when present:

- active state, interval / retry_interval / maxretries / timeout
- upside_down, resend_interval, ignore_tls (→ `tls_ignore`)
- accepted status codes, weight, description
- `expiry_notification` → `cert_expiry_notify` (Sprint C)
- proxy_id (when proxy row is imported)
- parent folder links (group monitors → `monitor_groups`; checkable monitors get `group_id`)

### Notifications (Kuma type → Phoenix provider)

Only Phoenix's locked **11** providers are imported. Config keys are remapped from
Kuma camelCase into Phoenix snake_case (for example `telegramBotToken` →
`bot_token`).

| Kuma type(s) | Phoenix type |
|---|---|
| `telegram` | `telegram` |
| `discord` | `discord` |
| `slack` | `slack` |
| `smtp` / `email` / `mail` | `smtp` |
| `webhook` | `webhook` |
| `teams` / `msteams` / `teamsWebhook` | `teams` |
| `mattermost` | `mattermost` |
| `gotify` | `gotify` |
| `bark` | `bark` |
| `feishu` / `lark` | `feishu` |
| `line` / `lineNotify` / … | `line` |
| all others (PagerDuty, ntfy, Signal, …) | **skipped** |

Monitor↔notification links are kept only when **both** ends import successfully.

### Other entities

| Entity | Behaviour |
|---|---|
| Tags + monitor_tag | Imported |
| Proxies | Imported (including password) |
| Status pages | Imported (slug/title/theme/password hash/footer/CSS/show_tags) |
| Status page monitor order | Via Kuma `group` + `monitor_group` when `status_page_id` present |
| Status page CNAMEs | Imported |
| Maintenance windows | `single` and `cron` strategies when present; IANA `timezone` when present |
| Maintenance↔monitor links | Imported when both ends import |
| Users / password hashes | **Never imported** |
| API keys / sessions | **Never imported** |
| Heartbeats / history | **Never imported** |
| Incidents | **Never imported** |
| Status-page email subscribers | **Never imported** (re-subscribe in Phoenix) |

Unsupported entities are reported with source ID, type, and reason. Nothing is
silently coerced into a different Phoenix type.

## Admin import step

After conversion:

1. Sign in to Phoenix as an **admin** (session JWT).
2. Import the JSON with the existing backup import endpoint (same as a Phoenix
   export — see `docs/RUNBOOK.md` §1.3):

```bash
umask 077
export PHOENIX_URL=https://phoenix.example.com
# obtain PHOENIX_TOKEN as in the runbook

curl --fail-with-body --silent --show-error \
  -H "Authorization: Bearer $PHOENIX_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @/secure/path/phoenix-backup.json \
  "$PHOENIX_URL/api/backup/import" \
  -o phoenix-import-summary.json

python3 -m json.tool phoenix-import-summary.json
```

3. Import is **merge-only**: it creates new entities and remaps source IDs. It
   does not overwrite or delete existing Phoenix data.
4. Effect-check monitors, notification delivery, folders, and status pages.
5. Securely delete `phoenix-backup.json` when finished.

## Schema variants

The converter inspects available columns on both engines:

- **SQLite:** `PRAGMA table_info`
- **MariaDB:** `INFORMATION_SCHEMA.COLUMNS` for `DATABASE()`

so both older Kuma databases (no `parent` / `timeout` / `description`) and newer
ones (folder nesting via `parent`, timeouts, expiry flags) convert. When a
column is absent, related fields are left at Phoenix defaults. Reserved table
names such as `group` are quoted per dialect (`"group"` vs `` `group` ``).

## What this is not

- Not a live sync or continuous migration
- Not an in-place writer into Kuma or Phoenix SQLite/MariaDB
- Not a full disaster-recovery tool (heartbeats and history are intentionally
  left behind — start fresh checks after import)
- Not a subscriber migration (re-collect status-page email subscribers in Phoenix)

## Tests

```bash
go test -race -count=1 ./internal/adapters/importer/uptimekuma/
```

SQLite fixtures cover classic and parent-aware schemas. Optional live MariaDB
conversion can be exercised by pointing a throwaway schema at the CLI with a
read-only DSN (never use the production Phoenix `phoenix` database).
