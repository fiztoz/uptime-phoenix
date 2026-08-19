# Running release binaries

How to download and run **Uptime Phoenix** binaries from GitHub Releases, and which
environment variables to set for each mode.

Release: [v0.3.1](https://github.com/fiztoz/uptime-phoenix/releases/tag/v0.3.1)

Also see: [Helm & Argo CD](helm-and-argocd.md) · [Docker / GHCR](docker-ghcr.md) · [DEPLOYMENT_MODES.md](../DEPLOYMENT_MODES.md)

---

## Binaries in a release

| Asset pattern | Role |
|---|---|
| `uptime-phoenix_<ver>_<os>_<arch>` | All-in-one (`MODE=all` by default) — HTTP + WS + scheduler + embedded UI |
| `uptime-phoenix-api_<ver>_<os>_<arch>` | API only (`MODE=api`) — HTTP + WS, runs migrations |
| `uptime-phoenix-worker_<ver>_<os>_<arch>` | Worker only (`MODE=worker`) — scheduler, checkers, notifier (no HTTP) |
| `uptime-phoenix-config_<ver>_…` | Config-as-code CLI (`phoenix-config`) |
| `uptime-phoenix-kuma-import_<ver>_…` | Uptime Kuma import helper |

**OS / arch:** `linux` / `darwin` / `windows` × `amd64` / `arm64`.

Same release also has `SHA256SUMS` and `INVENTORY.md`.

### Download example (Linux amd64, v0.3.1)

```bash
VER=0.3.1
BASE=https://github.com/fiztoz/uptime-phoenix/releases/download/v${VER}

curl -fsSL -O "${BASE}/uptime-phoenix_${VER}_linux_amd64"
curl -fsSL -O "${BASE}/SHA256SUMS"
sha256sum -c SHA256SUMS --ignore-missing

chmod +x "uptime-phoenix_${VER}_linux_amd64"
sudo mv "uptime-phoenix_${VER}_linux_amd64" /usr/local/bin/uptime-phoenix
```

API / worker:

```bash
curl -fsSL -O "${BASE}/uptime-phoenix-api_${VER}_linux_amd64"
curl -fsSL -O "${BASE}/uptime-phoenix-worker_${VER}_linux_amd64"
chmod +x uptime-phoenix-api_${VER}_linux_amd64 uptime-phoenix-worker_${VER}_linux_amd64
```

---

## Modes

| Mode | Binary | Purpose |
|---|---|---|
| **all** (default) | `uptime-phoenix` | Single process: API + worker + embedded frontend |
| **api** | `uptime-phoenix-api` (or `uptime-phoenix` with `MODE=api`) | HTTP + WebSocket only; runs DB migrations |
| **worker** | `uptime-phoenix-worker` (or `uptime-phoenix` with `MODE=worker`) | Checks + notifications; no listen port |

- All-in-one: SQLite is fine; Redis not required.
- Split (`api` + `worker`): use **shared MariaDB** + **`REDIS_URL`** so the worker’s
  events reach the API’s WebSocket hub. Start **api first** (migrations), then worker.

---

## Environment variables

Core settings (from `internal/bootstrap/config.go`):

| Variable | Default | Description |
|---|---|---|
| `MODE` | `all` | `all` \| `api` \| `worker` (ignored by `uptime-phoenix-api` / `-worker` entrypoints that hard-code mode) |
| `HOST` | `0.0.0.0` | HTTP bind address (api / all) |
| `PORT` | `3000` | HTTP port (api / all) |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `DB_ENGINE` | `sqlite` | `sqlite` \| `mariadb` |
| `DB_DSN` | SQLite file DSN | Connection string (see examples below) |
| `JWT_SECRET` | `change-me-in-production` | **Set a strong secret in production** |
| `JWT_EXPIRE_HOURS` | `24` | Session lifetime in hours (must be `> 0`) |
| `BOOTSTRAP_USERNAME` | _(empty)_ | Create first admin if no users exist |
| `BOOTSTRAP_PASSWORD` | _(empty)_ | Password for bootstrap admin (min 8 chars) |
| `REDIS_URL` | _(empty)_ | e.g. `redis://127.0.0.1:6379/0` — required for split live UI |
| `PUBLIC_URL` | _(empty)_ | Public origin, e.g. `https://uptime.example.com` (OIDC / absolute links) |
| `TOTP_ISSUER` | `Phoenix` | TOTP label |
| `PRODUCTION` | `false` | Tighten production defaults when `true` |
| `CORS_ALLOW_ORIGINS` | _(empty)_ | Comma-separated browser origins if UI is on another host |
| `WS_ALLOWED_ORIGINS` | _(empty)_ | Extra WebSocket origin host patterns |
| `WORKER_ID` | _(empty)_ | Unique id per worker pod when sharding |
| `SHARD_BATCH_SIZE` | `200` | Monitors per worker (sharded) |
| `SHARD_LEASE_TTL` | `300` | Lease TTL seconds |
| `HEARTBEAT_RETENTION_DAYS` | `180` | Retention for raw heartbeats |

OIDC-related vars (`OIDC_ISSUER`, `OIDC_CLIENT_ID`, …) are documented in the architecture /
OIDC contracts; leave unset unless enabling SSO.

### DSN examples

```bash
# SQLite (default path relative to process cwd)
export DB_ENGINE=sqlite
export DB_DSN='file:phoenix.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)'

# MariaDB
export DB_ENGINE=mariadb
export DB_DSN='phoenix:SECRET@tcp(127.0.0.1:3306)/phoenix?parseTime=true&loc=UTC&multiStatements=true'
```

---

## Mode: all-in-one (recommended start)

One process, embedded UI, SQLite, no Redis.

```bash
export JWT_SECRET="$(openssl rand -hex 32)"
export BOOTSTRAP_USERNAME=admin
export BOOTSTRAP_PASSWORD='ChangeMe123!'
export DB_ENGINE=sqlite
export DB_DSN='file:/var/lib/uptime-phoenix/phoenix.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)'
export HOST=0.0.0.0
export PORT=3000
export LOG_LEVEL=info
# optional: export PUBLIC_URL=https://uptime.example.com

mkdir -p /var/lib/uptime-phoenix
uptime-phoenix
# open http://localhost:3000
```

systemd unit sketch:

```ini
[Unit]
Description=Uptime Phoenix
After=network.target

[Service]
Type=simple
User=uptime-phoenix
WorkingDirectory=/var/lib/uptime-phoenix
Environment=JWT_SECRET=replace-me
Environment=BOOTSTRAP_USERNAME=admin
Environment=BOOTSTRAP_PASSWORD=ChangeMe123!
Environment=DB_ENGINE=sqlite
Environment=DB_DSN=file:/var/lib/uptime-phoenix/phoenix.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)
Environment=PORT=3000
Environment=HOST=0.0.0.0
ExecStart=/usr/local/bin/uptime-phoenix
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

---

## Mode: split (api + worker)

Needs **MariaDB** and **Redis**. Same `JWT_SECRET` and `DB_DSN` on both processes.

### 1) Start API (migrations + HTTP)

```bash
export MODE=api   # only needed if using the all-in-one binary
export JWT_SECRET="$(openssl rand -hex 32)"
export DB_ENGINE=mariadb
export DB_DSN='phoenix:SECRET@tcp(127.0.0.1:3306)/phoenix?parseTime=true&loc=UTC&multiStatements=true'
export REDIS_URL='redis://127.0.0.1:6379/0'
export BOOTSTRAP_USERNAME=admin
export BOOTSTRAP_PASSWORD='ChangeMe123!'
export HOST=0.0.0.0
export PORT=3000
export PUBLIC_URL='https://uptime.example.com'

./uptime-phoenix-api_0.3.1_linux_amd64
# wait until /api/health/ready is OK
```

### 2) Start worker (after API is ready)

```bash
export JWT_SECRET=…          # same as API
export DB_ENGINE=mariadb
export DB_DSN=…              # same as API
export REDIS_URL='redis://127.0.0.1:6379/0'
export LOG_LEVEL=info
# optional sharding:
# export WORKER_ID=worker-1

./uptime-phoenix-worker_0.3.1_linux_amd64
```

### 3) Frontend in split binary setups

- **All-in-one binary** embeds the SPA; browsers hit the API port directly.
- **API-only binary** still serves the embedded SPA on the same HTTP port in the
  default build (static assets are compiled into the Go binary). A separate
  nginx/`uptime-phoenix-web` image is only required for a dedicated web tier
  (Helm `web.split` / compose split stack).

Health checks:

```bash
curl -sS http://127.0.0.1:3000/api/health/live
curl -sS http://127.0.0.1:3000/api/health/ready
```

---

## Using the all-in-one binary as api/worker

```bash
# API
MODE=api DB_ENGINE=mariadb DB_DSN=… REDIS_URL=… JWT_SECRET=… ./uptime-phoenix

# Worker
MODE=worker DB_ENGINE=mariadb DB_DSN=… REDIS_URL=… JWT_SECRET=… ./uptime-phoenix
```

Prefer the dedicated `uptime-phoenix-api` / `uptime-phoenix-worker` release assets
when you want hard-coded roles without relying on `MODE`.

---

## Config CLI & Kuma import

```bash
# Config-as-code (see docs / phoenix.dev/v1 schema)
./uptime-phoenix-config_0.3.1_linux_amd64 --help

# Kuma import helper
./uptime-phoenix-kuma-import_0.3.1_linux_amd64 --help
```

---

## Checklist

1. Download the right **os/arch** asset and verify **SHA256SUMS**.
2. Set a strong **`JWT_SECRET`**.
3. Set **`BOOTSTRAP_*`** only for first run (or create the first user via UI bootstrap).
4. All-in-one: SQLite path must be writable.
5. Split: shared MariaDB + Redis; API up before worker.
6. Put a reverse proxy / TLS in front for production (`PUBLIC_URL` should match).
