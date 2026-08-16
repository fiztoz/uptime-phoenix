# Uptime Phoenix — Testing Instructions for Agents

> **Purpose:** This document tells any agent (AI or human) exactly how to verify
> Uptime Phoenix changes before marking a task complete. Follow every applicable gate.

> **CI is restored (owner, 2026-07-28).** `.github/workflows/ci.yml` runs the gate on
> every `pull_request` and push to `main` (backend, frontend, e2e, MariaDB contract,
> Helm, Docker, actionlint). Local `make gate-full` remains **required for thoroughness**
> and works fully offline — do not treat a green CI check as a substitute for running the
> local gate before you merge your own work.
>
> **Gate debt (cleared 2026-07-27):** the repository gate-debt sprint took Go lint from
> ~158 → 0 findings, frontend prettier/eslint baseline to clean, and `govulncheck`
> reachable stdlib issues to 0 via the Go 1.25.12 toolchain pin (see `docs/ROADMAP.md`).
> New work must not reintroduce that debt — `make gate-full` and CI should stay green.

---

## Table of Contents

1. [Quick Reference — Gate Commands](#1-quick-reference--gate-commands)
2. [Backend Tests (Go)](#2-backend-tests-go)
3. [Frontend Checks (Svelte)](#3-frontend-checks-svelte)
4. [Linting](#4-linting)
5. [Build Verification](#5-build-verification)
6. [Docker Compose Smoke Test](#6-docker-compose-smoke-test)
7. [Helm Chart Validation](#7-helm-chart-validation)
8. [Playwright E2E Tests](#8-playwright-e2e-tests)
9. [Manual Testing Checklist](#9-manual-testing-checklist)
10. [Regression Checklist by Area](#10-regression-checklist-by-area)
11. [Common Failures & Fixes](#11-common-failures--fixes)

---

## 1. Quick Reference — Gate Commands

Run these in order after ANY code change. All must pass. The single command that runs
all of them is `make gate-full` (see the Makefile) — use the individual commands below
when you only touched one area and want faster feedback.

```bash
# From project root

# 1. Go build (catches compile errors)
go build ./...

# 2. Go vet + gofmt
go vet ./internal/...
gofmt -l internal/    # must print nothing

# 3. Go tests (unit + integration, race detector)
go test -race -count=1 ./...

# 4. Go lint
golangci-lint run
# If not installed: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# 5. Go vulnerability scan
govulncheck ./...
# If not installed: go install golang.org/x/vuln/cmd/govulncheck@latest

# 6. Frontend type-check + tests + build
cd web && bun run check && bun run test && bun run build

# 7. Frontend lint (prettier + eslint)
cd web && bun run lint

# 8. Playwright E2E (spins up its own server; no external instance needed)
cd web && bun run test:e2e

# 9. Helm lint + template
helm lint charts/uptime-phoenix
helm template uptime-phoenix charts/uptime-phoenix

# 10. Whitespace/conflict-marker check
git diff --check
```

Or, in one shot:

```bash
make gate-full
```

`make gate-full` does **not** include the MariaDB repository contract (needs
`TEST_MARIADB_DSN` against a real database), the fresh-DB smoke suites under
`scripts/`, or the k6 load ramp — those need external services and are documented
separately (§2 and §7 below). CI runs the MariaDB contract in the `mariadb-contract`
job (`phoenix_ci` throwaway DB). Run the smoke suites and k6 before a release.

**Do NOT report a task complete until all applicable gates pass.** CI covers PR/main;
you still own the local gate for work-in-progress and offline verification.

---

## 2. Backend Tests (Go)

### 2.1 Run All Tests

```bash
cd /path/to/uptime-phoenix
go test -race -count=1 ./...
```

- `-race` enables the Go race detector — catches data races in concurrent code
- `-count=1` disables test caching — always run fresh
- Expected: `Go test: NNN passed in M packages` with 0 failures

### 2.2 Run Specific Package Tests

```bash
# Only core services
go test -v ./internal/core/services/...

# Only a specific test
go test -v -run TestRollup1m ./internal/core/services/

# Checker tests
go test -v ./internal/adapters/checker/...

# Uptime Kuma importer (SQLite fixtures; engine=sqlite default)
go test -race -count=1 ./internal/adapters/importer/uptimekuma/

# Manual Kuma conversion (never against production Phoenix DB):
#   go run ./cmd/kuma-import --input /path/kuma.db --output /tmp/phx.json
#   go run ./cmd/kuma-import --engine mariadb --dsn "$KUMA_RO_DSN" --output /tmp/phx.json
# See docs/KUMA-IMPORT.md.

# Certificate-alert + maintenance timezone unit tests live under:
#   go test -race -count=1 ./internal/core/services/ -run 'Certificate|Maintenance|Timezone|Cron'
# Subscription + OG meta:
#   go test -race -count=1 ./internal/core/services/ -run 'Subscribe|NotifyIncident|NotifyMaintenance'
#   go test -race -count=1 ./internal/adapters/http/ -run 'StatusPageMeta|Inject'
#   go test -race -count=1 ./internal/adapters/auth/ -run 'SubscriberToken'

# Repository tests
go test -v ./internal/adapters/repository/...

# HTTP handler tests
go test -v ./internal/adapters/http/...

# Middleware tests
go test -v ./internal/adapters/http/middleware/...
```

### 2.3 Run with Coverage

```bash
go test -race -count=1 -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | tail -1
# Shows: total: (statements) XX.X%
```

### 2.4 Test File Locations

| Area | Test Files |
|---|---|
| Domain services | `internal/core/services/*_test.go` |
| Auth | `internal/core/services/auth_service_test.go` |
| Heartbeat | `internal/core/services/heartbeat_service_test.go` |
| Aggregate | `internal/core/services/aggregate_service_test.go` |
| HTTP checker | `internal/adapters/checker/http_test.go` |
| DNS checker | `internal/adapters/checker/dns_test.go` |
| Monitor handler | `internal/adapters/http/handlers/monitor_test.go` |
| Auth handler | `internal/adapters/http/handlers/auth_test.go` |
| Rate limit | `internal/adapters/http/middleware/ratelimit_test.go` |
| Request ID | `internal/adapters/http/middleware/requestid_test.go` |
| WebSocket wire | `internal/adapters/ws/wire_test.go` |

### 2.5 Writing New Tests

Follow existing patterns in the same package. Key rules:
- **Domain services:** mock ports with in-memory fakes (no DB, no HTTP)
- **Adapters:** integration tests with real DB when possible
- **Every exported function** should have at least one test
- Test names: `TestFunctionName_Scenario` (e.g., `TestRollup1m_EmptyHeartbeats`)

---

## 3. Frontend Checks (Svelte)

### 3.1 Build

```bash
cd web
bun install --frozen-lockfile   # Install deps (if node_modules missing)
bun run build                   # Production build — catches TS errors, missing imports
```

Expected: `✓ built in X.XXs` with no errors. Warnings about pre-existing
issues (paraglide, async_hooks) are acceptable.

### 3.2 Type Check

```bash
cd web
bunx svelte-kit sync && bunx svelte-check --tsconfig ./tsconfig.json
```

Known pre-existing errors (NOT blockers):
- `Cannot find name 'process'` in test files — missing `@types/node`
- `File .../paraglide/messages/_index.js is not a module` — paraglide build artifact
- `.ts extension` warnings — SvelteKit handles this

**Blockers:** any error in `src/routes/` or `src/lib/` (NOT in `tests/` or `node_modules/`).

### 3.3 Lint

```bash
cd web
bun run lint    # prettier --check . && eslint .
```

To auto-fix formatting:
```bash
bun run format  # prettier --write .
```

---

## 4. Linting

### 4.1 Go (golangci-lint)

```bash
cd /path/to/uptime-phoenix
golangci-lint run
```

Install if missing:
```bash
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
```

Common warnings to fix:
- `unparam` — unused function parameters → use `_` or remove
- `SA1029` — using string as context key → define custom type
- `SA1019` — using deprecated API → find replacement
- `errcheck` — unchecked errors → add `if err != nil`

Pre-existing warnings (do NOT fix unless touching that file):
- `grpc.go` — deprecated `grpc.DialContext` / `grpc.WithBlock`
- `ws.go` — string context key for JWT

### 4.2 Frontend (ESLint + Prettier)

```bash
cd web
bun run lint
bun run format  # auto-fix
```

---

## 5. Build Verification

### 5.1 Go Binary (CGO_ENABLED=0)

```bash
cd /path/to/uptime-phoenix
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/uptime-phoenix ./cmd/app
```

This verifies:
- No CGO dependency slipped in
- All imports resolve
- The binary compiles for the target platform

### 5.2 Frontend Production Build

```bash
cd web
bun install --frozen-lockfile && bun run build
```

Output goes to `web/build/` (or `.svelte-kit/output/`). This is what gets
embedded into the Go binary via `//go:embed web/dist`.

### 5.3 Full Stack Build

```bash
cd /path/to/uptime-phoenix
make build
```

This runs both `build-backend` and `build-frontend`.

---

## 6. Docker Compose Smoke Test

```bash
cd /path/to/uptime-phoenix

# Build and start (detached)
docker compose up -d --build

# Wait for healthy
docker compose ps    # Both should show "healthy"

# Test health endpoint
curl -sf http://localhost:3000/api/health/live
# Expected: {"status":"ok"}

# Test bootstrap login
curl -sf -X POST http://localhost:3000/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"ChangeMe123!"}'
# Expected: {"token":"..."}

# Open in browser
open http://localhost:3000

# Cleanup
docker compose down -v
```

### Environment Variables (defaults in docker-compose.yml)

| Variable | Default | Description |
|---|---|---|
| `DB_ENGINE` | `mariadb` | `mariadb` or `sqlite` |
| `DB_DSN` | auto-set | Connection string |
| `BOOTSTRAP_USERNAME` | `admin` | First user |
| `BOOTSTRAP_PASSWORD` | `ChangeMe123!` | First password |
| `JWT_SECRET` | auto-set | JWT signing key |
| `LOG_LEVEL` | `debug` | `debug/info/warn/error` |
| `MODE` | `all` | `all/api/worker` |

---

## 7. Helm Chart Validation

```bash
cd /path/to/uptime-phoenix

# Lint
helm lint charts/uptime-phoenix

# Template (dry-run, single mode)
helm template uptime-phoenix charts/uptime-phoenix

# Template (multi-pod mode)
helm template uptime-phoenix charts/uptime-phoenix \
  --set scaling.mode=multi \
  --set redis.enabled=true \
  --set redis.host=redis.example.internal

# Template (CPU HPA; add hpa.wsConnections.enabled=true only with prometheus-adapter)
helm template uptime-phoenix charts/uptime-phoenix \
  --set mode=api \
  --set hpa.enabled=true

# Template (official Valkey subchart)
helm template uptime-phoenix charts/uptime-phoenix \
  --set mode=split \
  --set database.engine=mariadb \
  --set mariadb.enabled=true \
  --set valkey.enabled=true

# Template (production split + shards + Valkey overlay)
helm template uptime-phoenix charts/uptime-phoenix \
  -f charts/uptime-phoenix/values-production-split.yaml \
  --set mariadbExternal.password=ci

# Template (external Redis URL from an existing Secret)
helm template uptime-phoenix charts/uptime-phoenix \
  --set redis.enabled=true \
  --set redis.existingSecret=phoenix-redis

# Template (MariaDB mode)
helm template uptime-phoenix charts/uptime-phoenix \
  --set database.engine=mariadb \
  --set mariadb.enabled=true
```

**Verify:**
- No template rendering errors
- All YAML is valid
- ConfigMap has correct env vars
- Deployment has correct image, ports, probes
- API / worker / all-in-one pod templates have `checksum/config` and
  `checksum/secret` annotations; changing `config.logLevel` (or a secret
  value such as `oidc.clientSecret`) changes those hashes
- Internal Redis mode renders a Redis StatefulSet/Service/Secret and Phoenix's
  `REDIS_URL` references the Secret's `uri` key
- Service matches Deployment ports

---

## 8. Playwright E2E Tests

### 8.1 Prerequisites

```bash
cd web
bun install --frozen-lockfile
bunx playwright install --with-deps chromium
```

### 8.2 Running E2E Tests

**Requires a running Uptime Phoenix instance** (either `docker compose up` or `make run`).

```bash
cd web

# Set the API URL (default: http://localhost:3000)
export PHOENIX_API_URL=http://localhost:3000

# Run all E2E tests
bun run test:e2e

# Run specific test
npx playwright test --config=tests/e2e.config.ts tests/e2e/01-auth.spec.ts

# Run with UI (interactive)
npx playwright test --config=tests/e2e.config.ts --ui

# Run headed (see browser)
npx playwright test --config=tests/e2e.config.ts --headed
```

### 8.3 Existing E2E Specs

| Spec | What It Tests |
|---|---|
| `01-auth.spec.ts` | Login flow, JWT, 2FA challenge |
| `02-monitor-crud.spec.ts` | Create/edit/delete monitors |

### 8.4 Writing New E2E Tests

Create a new file in `web/tests/e2e/` following the naming pattern `NN-descriptive-name.spec.ts`.

```typescript
import { test, expect } from '@playwright/test';
import { login, API_BASE } from './helpers';

test.describe('Feature Name', () => {
  test.beforeEach(async ({ page }) => {
    await login(page); // Login first
  });

  test('should do something', async ({ page }) => {
    await page.goto('/dashboard');
    await expect(page.locator('h1')).toHaveText('Dashboard');
  });
});
```

---

## 9. Manual Testing Checklist

When automated tests aren't sufficient, verify these flows manually.

### 9.1 Auth Flow
- [ ] Register a new user
- [ ] Login with correct credentials
- [ ] Login with wrong password → error message
- [ ] Enable 2FA → scan QR → verify TOTP code
- [ ] Logout → redirected to login page
- [ ] JWT expires → redirected to login

### 9.2 Monitor CRUD
- [ ] Create HTTP monitor (https://example.com, GET, 200-299)
- [ ] Create TCP monitor (hostname:port)
- [ ] Create Ping monitor (requires ICMP permissions on Linux)
- [ ] Edit monitor name/config
- [ ] Delete monitor
- [ ] Monitor appears on dashboard with status pill
- [ ] Heartbeats appear after check interval

### 9.3 Dashboard
- [ ] Stats cards show correct counts (total/up/down)
- [ ] Monitor cards update in real-time via WebSocket
- [ ] Connection indicator shows "connected"
- [ ] Reconnection works after server restart

### 9.4 Notifications
- [ ] Add Telegram notification (bot_token + chat_id)
- [ ] Add Discord notification (webhook_url)
- [ ] Add Webhook notification (custom URL)
- [ ] Create Discord, SMTP, Webhook, and LINE message templates; insert Uptime Phoenix variables and verify the rendered preview
- [ ] Discord template: customize UP/DOWN/PENDING/MAINTENANCE/certificate colors, title link, footer, timestamp, and ordered inline/full-width fields
- [ ] Switch the Discord preview between Monitor alert and Group alert; monitor-only fields disappear for groups and group condition/threshold fields disappear for monitors
- [ ] Send monitor and folder transitions through the same Discord template; verify the delivered embed matches the preview structure and uses the correct scope variables
- [ ] Create an SMTP HTML template with a plain-text fallback; verify the preview switches between desktop, mobile, and fallback text without loading remote images
- [ ] Send monitor and folder transitions through the SMTP HTML template; verify the message is `multipart/alternative`, dynamic values are escaped in HTML, and both MIME parts contain the correct scope values
- [ ] Preview and deliver a monitor recovery: `started_at`, `duration`, and `tags` match the persisted outage/tag data; switch to a folder alert and verify lifecycle, tag, and acknowledgement-only values are empty rather than sample data
- [ ] Reopen a legacy SMTP template with no provider config; verify it remains plain text and delivers exactly one `text/plain` body
- [ ] Select a matching template while creating/editing each supported notification; mismatched providers are not offered and the API rejects them
- [ ] Delete a selected template → the notification falls back to the provider default layout
- [ ] Webhook `json.*` variables remain valid JSON when monitor/message values contain quotes
- [ ] Test-send button fires test notification
- [ ] Edit notification config
- [ ] Delete notification
- [ ] Assign notification to monitor
- [ ] Monitor goes DOWN → notification fires

### 9.5 Status Pages
- [ ] Create status page (slug, title, description)
- [ ] Assign monitors to status page
- [ ] Create incident on status page
- [ ] Resolve incident
- [ ] Active incident cannot be deleted; after resolution an admin can delete it from incident history
- [ ] View public status page at `/{slug}`
- [ ] Public page shows monitor statuses + uptime bars
- [ ] Public page shows monitors before a compact active-only incident section
- [ ] Resolved incidents do not consume space on the public status page
- [ ] Password-protected page requires access code
- [ ] Custom domain resolves correctly (if configured)

### 9.6 Aggregate Rollups
- [ ] After 1 minute: `heartbeat_1m` table has data
- [ ] After 10 minutes: `heartbeat_1h` table has data
- [ ] After 1 hour: `heartbeat_1d` table has data
- [ ] Uptime percentage is NOT hardcoded 100% (shows real data)

### 9.7 Settings
- [ ] Update user profile
- [ ] Change password
- [ ] Enable/disable 2FA
- [ ] App settings persist

### 9.8 Maintenance Windows
- [ ] Create maintenance window (single or cron)
- [ ] Monitor shows "maintenance" status during window
- [ ] Notifications suppressed during maintenance

---

## 10. Regression Checklist by Area

When you change code in a specific area, run the corresponding checks.

### If you changed `internal/core/domain/`:
```bash
go build ./...                              # Domain compiles
go test ./internal/core/services/...        # Services still work
go test ./internal/adapters/...             # Adapters still work
```

### If you changed `internal/core/ports/`:
```bash
go build ./...                              # Everything compiles
go test ./...                               # All tests pass
```

### If you changed `internal/core/services/`:
```bash
go test -v ./internal/core/services/...     # Service tests pass
go build ./...                              # Adapters compile
```

### If you changed `internal/adapters/checker/`:
```bash
go test -v ./internal/adapters/checker/...  # Checker tests pass
go build ./...                              # Everything compiles
```

Database capacity conditions (`check_session_pool` / `check_storage`) must test
effects, not only messages/status codes:

- `Validate` accepts only `ping` / `select_1`; no operator SQL is executed.
- Primary connect/ping/select failure is DOWN and emits no speculative capacity row.
- Over-threshold stays heartbeat UP and emits typed condition `warning` only after two consecutive samples.
- A `74%` then `77%` sequence after an 80% warning must **not** recover (hysteresis uses the stable state).
- Query/privilege failure stays heartbeat UP and emits condition `error` (never a silent skip).
- `LatencyMs` stops after the primary probe; `DurationMs` includes auxiliary queries.
- Capacity queries run on every primary check. Recommend a 30s+ monitor interval on busy engines.
- Warning/error and recovery require two consecutive samples; recovery also crosses the 5-point hysteresis boundary.
- The first warning/error sample stays unconfirmed (`state` empty): no chip, no REST row, no notify.
- Typed `condition.delete` must reach a memory-bus WebSocket client; REST snapshots must not overwrite newer live updates.
- Paused and maintenance monitors do not enter Needs attention merely because an unsampled condition becomes stale.
- Maintenance suppresses send without marking delivered; an all-channel failure remains retryable.
- Repository/API/WS tests assert UTC freshness, RBAC filtering, snake-case views, cursor secrecy, and stale derivation.
- Availability Insights, uptime, folders, badges, and public status are unchanged by a capacity warning.
- Dashboard **Card: Capacity** replaces the ping sparkline with session/storage meters on monitors that have conditions; other cards stay on response. The same preference applies to the wallboard.

### If you changed `internal/adapters/http/handlers/`:
```bash
go test -v ./internal/adapters/http/...     # Handler tests pass
go build ./...                              # Router compiles
```

### If you changed `internal/adapters/repository/`:
```bash
go test -v ./internal/adapters/repository/...  # Repo tests pass
go build ./...                                 # Everything compiles
```

### If you changed `web/src/routes/`:
```bash
cd web && bun run build                     # Build succeeds
cd web && bun run lint                      # Lint passes
```

### If you changed `web/src/lib/components/`:
```bash
cd web && bun run build                     # Build succeeds
cd web && bun run lint                      # Lint passes
```

### If you changed `web/src/lib/stores/`:
```bash
cd web && bun run build                     # Build succeeds
```

### If you changed `charts/`:
```bash
helm lint charts/uptime-phoenix                    # Chart is valid
helm template uptime-phoenix charts/uptime-phoenix        # Templates render
```

---

## 11. Common Failures & Fixes

### `go build` fails with import cycle
**Cause:** Code in `internal/core/` imported an adapter.
**Fix:** The hexagonal boundary is violated. Move the logic to the service layer
and use a port interface.

### `go test` fails with "interface not satisfied"
**Cause:** A mock/fake doesn't implement all methods of a port interface.
**Fix:** Check the interface definition in `internal/core/ports/` and add the
missing methods to your test double.

### `golangci-lint` reports `unparam` on new code
**Cause:** A function parameter is unused.
**Fix:** Use `_` for unused params: `func Foo(ctx context.Context, _ string) error`

### `bun run build` fails with "Cannot find module"
**Cause:** Missing import or package not installed.
**Fix:** `cd web && bun install --frozen-lockfile` then retry. If still failing, check the import path.

### `helm lint` fails
**Cause:** Template syntax error or invalid values.
**Fix:** `helm template uptime-phoenix charts/uptime-phoenix --debug` for detailed error output.

### Playwright test times out
**Cause:** Uptime Phoenix server not running or wrong URL.
**Fix:** Ensure `docker compose up` or `make run` is running, and
`PHOENIX_API_URL` is set correctly.

### Docker build fails on `go:embed`
**Cause:** Frontend build output not at `web/dist/`.
**Fix:** The Dockerfile must run `bun install --frozen-lockfile && bun run build` in the web/ directory
before the Go build stage. Check the Dockerfile multi-stage setup.

---

## Checklist for Agents

Before reporting ANY task complete, run through this. CI catches failures on PR/main,
but you still own this list for local work and for areas CI does not cover (smoke
suites, load, manual UI).

- [ ] `go build ./...` passes
- [ ] `go test -race -count=1 ./...` passes (all tests)
- [ ] `golangci-lint run` has zero warnings on NEW code (the codebase as a whole
      currently has pre-existing findings — see the banner at the top of this file;
      do not let new code add to that count, and do not feel obligated to fix
      unrelated pre-existing findings outside files you're already touching)
- [ ] `govulncheck ./...` reports no NEW reachable vulnerability introduced by your
      change (the toolchain/stdlib backlog described in the banner above is a
      separate, known issue)
- [ ] `cd web && bun run build` passes (if you touched frontend)
- [ ] `cd web && bun run lint` passes (if you touched frontend)
- [ ] `cd web && bun run test:e2e` passes (if you touched a critical user journey)
- [ ] `helm lint charts/uptime-phoenix` and `helm template uptime-phoenix charts/uptime-phoenix` pass
      (if you touched charts)
- [ ] No framework imports in `internal/core/` (hexagonal boundary)
- [ ] No new external dependencies without checking CGO-free
- [ ] If you added a monitor type: one file + one line in `registry.go`
- [ ] If you added a notification provider: one file + one line in `registry.go`
- [ ] If you touched the database: migration files (up + down) exist
- [ ] Error wrapping: `fmt.Errorf("doing X: %w", err)` preserves the chain
- [ ] Every exported function has a doc comment
- [ ] If this is a release: follow `docs/RELEASING.md` — release is a local, manual,
      owner-triggered procedure, never automated
