# Uptime Phoenix — Local Development Guide

Uptime Phoenix is a self-hosted monitoring tool (Go backend + Svelte 5 frontend). The default local setup uses **SQLite** and needs **no external services**.

---

## Prerequisites

| Tool | Version |
|------|---------|
| Go | 1.25+ |
| Bun | 1.0+ |

Optional (for hot-reload backend):

```bash
go install github.com/air-verse/air@latest
```

---

## Quick Start (recommended)

Single binary with embedded UI — simplest way to run locally:

```bash
make run
```

Then open **http://localhost:3000** and log in:

| Field | Value |
|-------|-------|
| Username | `admin` |
| Password | `ChangeMe123!` |

The bootstrap user is created automatically on first launch when no users exist.

To reset and start fresh:

```bash
rm -f phoenix.db
make run
```

---

## Development Mode (hot-reload)

For active frontend/backend development:

**Terminal 1 — backend (port 3000):**

```bash
make dev-backend
```

**Terminal 2 — frontend (port 5173):**

```bash
make dev-frontend
```

Open **http://localhost:5173**. The Vite dev server proxies `/api` and `/ws` to the Go backend on port 3000.

Or run both in one terminal:

```bash
make dev
```

---

## Manual Build & Run

```bash
# Build everything
make build

# Run with explicit env vars
DB_ENGINE=sqlite \
DB_DSN='file:phoenix.db?_pragma=foreign_keys(1)' \
BOOTSTRAP_USERNAME=admin \
BOOTSTRAP_PASSWORD=ChangeMe123! \
MODE=all \
PORT=3000 \
./bin/uptime-phoenix
```

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `3000` | HTTP listen port |
| `HOST` | `0.0.0.0` | HTTP bind address |
| `DB_ENGINE` | `sqlite` | `sqlite` or `mariadb` |
| `DB_DSN` | `file:phoenix.db?...` | Database connection string |
| `BOOTSTRAP_USERNAME` | _(empty)_ | First-run admin username |
| `BOOTSTRAP_PASSWORD` | _(empty)_ | First-run admin password |
| `JWT_SECRET` | `change-me-in-production` | JWT signing key |
| `MODE` | `all` | `all`, `api`, or `worker` |
| `REDIS_URL` | _(empty)_ | Opt-in Redis EventBus |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

---

## Creating a Monitor

1. Log in at http://localhost:3000 (or :5173 in dev mode).
2. Go to **Monitors** → **Add Monitor**.
3. Fill in **Name** and type-specific fields (e.g. **URL** for HTTP monitors).
4. Click **Create Monitor**.

### Common errors

| Symptom | Cause | Fix |
|---------|-------|-----|
| `HTTP 404` on API calls | Frontend dev server running without backend | Start backend on port 3000 (`make dev-backend`) |
| `invalid config: url is required` | HTTP monitor missing URL | Fill in the URL field before saving |
| `authentication required` | Not logged in or expired token | Log in again |
| `internal error` (500) | Database or server issue | Check terminal logs; try `rm phoenix.db && make run` |

### API test (curl)

```bash
# Login
TOKEN=$(curl -s -X POST http://localhost:3000/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"ChangeMe123!"}' \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")

# Create HTTP monitor
curl -s -X POST http://localhost:3000/api/monitors \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{
    "name": "Example",
    "type": "http",
    "interval": 60,
    "timeout": 30,
    "config": {"url": "https://example.com"},
    "active": true
  }'
```

Expected response: `HTTP 201` with the created monitor JSON.

---

## Testing

```bash
# All Go tests
make test-backend

# Linters
make lint-backend

# Frontend lint (if you touched web/)
cd web && bun run lint
```

---

## Docker (optional)

```bash
make build-docker
make docker-run
```

Opens on http://localhost:3000.

---

## Troubleshooting

**Port already in use**

```bash
lsof -i :3000
kill <pid>
```

**Stale database**

```bash
rm -f phoenix.db
make run
```

**Frontend not updating in `make run`**

Rebuild the embedded UI:

```bash
make build-frontend
make run
```

**WebSocket not connecting in dev mode**

Ensure the backend is running on port 3000. The Vite proxy forwards `/ws` automatically.