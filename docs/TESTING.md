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

# Probe key provisioning: real filesystem creation/load/restart, concurrent
# no-replace publication, permissions and CLI; encrypted DB/key reopen runs
# SQLite and MariaDB when TEST_MARIADB_DSN is set.
go test -race -count=1 ./internal/adapters/auth ./cmd/phoenix-probe-key ./internal/adapters/repository -run 'ProbeSecretKey|KeyCommand|PreparedProbeConfigFileKey'

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
| WebSocket hub connect / `monitor.list` | `internal/adapters/ws/hub_monitorlist_connect_test.go` |

### 2.5 Writing New Tests

Follow existing patterns in the same package. Key rules:
- **Domain services:** mock ports with in-memory fakes (no DB, no HTTP)
- **Adapters:** integration tests with real DB when possible
- **Every exported function** should have at least one test
- Test names: `TestFunctionName_Scenario` (e.g., `TestRollup1m_EmptyHeartbeats`)

---

### 2.6 Colima multi-region runtime smoke

The local configuration semantic-validation contracts run with:

~~~bash
go test -race -count=1 ./internal/adapters/probe ./internal/core/services ./internal/adapters/repository -run 'LocalConfigValidation|ValidateNotificationTemplateDoesNot'
~~~

They cover the installed checker/provider inventory, the local token-free push
exception, effective checker overrides, disabled entries, structured templates,
timezone/cron/proxy syntax, unsupported capabilities, cancellation and secret-safe
errors. No checker `Check` or provider `Send` is invoked. With `TEST_MARIADB_DSN`
set, both engines prove exact encrypted revision validation after reconnect,
rejection of invalid newer content without fallback, and unchanged protected history.
Validation grants no activation or delivery authority; network/resource readiness,
current source/assignment fencing and durable activation require separate tests.

The local configuration construction contracts run with:

~~~bash
go test -race -count=1 ./internal/adapters/repository ./internal/adapters/probe ./internal/core/services -run 'LocalConfig'
~~~

Set `TEST_MARIADB_DSN` as below to execute both source-read engines. The tests
build protected snapshots from persisted assignments and dependencies, including
paused work, removed/re-added generations, inherited contact, disabled/empty
escalation policies, target visibility, templates, proxy credentials and exact
maintenance links. Unlinked and remote-only windows cover no local monitor.
A coordinated writer edits monitor and channel configuration between source reads;
the read transaction must return one complete version on both databases, including
MariaDB with a READ COMMITTED session default. Exact retries preserve ciphertext,
source changes at the same revision conflict, and a higher revision retains old
bytes. These tests do not activate configuration or authorize provider I/O.

The protected prepared-configuration contracts run with:

~~~bash
go test -race -count=1 ./internal/adapters/repository/... ./internal/adapters/auth ./internal/adapters/probe ./internal/core/services -run 'PreparedProbeConfig|ProbeConfig|ConfigInspector|ConfigSnapshot|ConfigTransfer|ProbeRegistryContract'
~~~

Set `TEST_MARIADB_DSN` as below to execute both engines. Migration 047 retains
complete original documents as AES-256-GCM ciphertext with authenticated identity
metadata. Tests cover exact-byte restart readback, local/remote decoder isolation,
randomized encryption, wrong-key/tamper rejection, immutable/idempotent revisions,
concurrent writers over two connections, stale revisions, changed authority,
failed-insert rollback, SQL constraints, migration cycles and guarded downgrade.
No snapshot is activated and no delivery work is created. Latest corruption must
fail without falling back to old credentials. The down migration refuses every
retained row; stop all writers for either direction. These tests inject a key;
durable key provisioning and runtime activation remain separate work.

The source alert identity and populated migration contracts run with:

~~~bash
go test -race -count=1 ./internal/adapters/repository/... -run 'AlertSource|RegionalAlert|SQLiteMigrationRebuild'
~~~

Migration 046 preserves legacy alert IDs/tokens and escalation progress while
adding stable source UUIDs and lifecycle versions. Contracts exercise failed
writes, concurrent/idempotent transitions, restart, version exhaustion, scoped
source lookup, deleted-ID preservation and a failed SQLite rebuild. Downgrade
refuses IDs referenced by regional incidents. Unreferenced mappings may reset on
an explicit downgrade/re-upgrade; stop all writers for either direction.

The atomic delivery-outbox storage contracts run with:

~~~bash
go test -race -count=1 ./internal/adapters/repository/... -run 'DeliveryOutbox|RegionalIncidents|LocalHeartbeat|ProbeRegistryContract'
~~~

Set `TEST_MARIADB_DSN` as below to run both engines. Migration 045 adds source
work without changing legacy alerts or heartbeat partitions. Tests inject failures
at incident, queue, outcome and completion writes; verify complete rollback,
expired-lease restart, stale-worker rejection, idempotent completion, due-time
boundaries, immutable context after history pruning, cross-probe/generation
isolation, concurrent claims from independent connections, and guarded downgrade.
The migration refuses downgrade with any queued work or source receipt, including
terminal rows. Stop all application writers for schema changes. The tests call
the storage ports directly; they do not establish a running provider consumer or
close the existing dispatcher's crash gap.

Assignment-scoped alert contracts run with:

~~~bash
go test -race -count=1 ./internal/adapters/repository/... -run 'RegionalAlert|SQLiteMigrationRebuild'
go test -race -count=1 ./internal/core/services -run 'Assignment|Alert|Escalation|Throttle'
~~~

Set TEST_MARIADB_DSN as below to execute both engines. Migration 044 tests
preserve populated alerts, acknowledgement metadata, escalation leases, foreign
keys and deleted-ID high-water marks across upgrade/downgrade. Remote or later
assignment-generation history blocks downgrade. SQLite rebuilds must execute
inside one transaction; startup does this automatically. Stop application writers
before schema changes on either engine.


Use a disposable MariaDB container and two separate databases: repository tests
truncate their database; the real-app smoke creates persistent test records in a
fresh database ending in `_smoke`. These example passwords are for this local
throwaway container only. Do not point either command at an existing deployment.

```bash
colima start
docker --context colima run -d --name phoenix-mr-validation \
  -p 127.0.0.1:43306:3306 --tmpfs /var/lib/mysql \
  -e MARIADB_ROOT_PASSWORD=phoenix-local-test-root \
  -e MARIADB_DATABASE=phoenix_ci -e MARIADB_USER=phoenix \
  -e MARIADB_PASSWORD=phoenix mariadb:11

# Wait until this succeeds before continuing.
docker --context colima exec phoenix-mr-validation \
  healthcheck.sh --connect --innodb_initialized
docker --context colima exec phoenix-mr-validation \
  mariadb -uroot -pphoenix-local-test-root -e \
  "CREATE DATABASE phoenix_mr_smoke; GRANT ALL ON phoenix_mr_smoke.* TO 'phoenix'@'%';"

GOTOOLCHAIN=go1.26.6 TEST_MARIADB_DSN='phoenix:phoenix@tcp(127.0.0.1:43306)/phoenix_ci?parseTime=true&loc=UTC&multiStatements=true' \
  go test -race -count=1 ./internal/adapters/repository/...
GOTOOLCHAIN=go1.26.6 go build -o /tmp/phoenix-mr-app ./cmd/app
DB_DSN='phoenix:phoenix@tcp(127.0.0.1:43306)/phoenix_mr_smoke?parseTime=true&loc=UTC&multiStatements=true' \
  python3 scripts/multi_region_smoke.py --app-binary /tmp/phoenix-mr-app

# Remove only the disposable container created above; keep the Colima VM.
docker --context colima rm -f phoenix-mr-validation
```

The environment name is exactly `TEST_MARIADB_DSN`. The unsupported spelling
`MARIADB_TEST_DSN` now makes the repository test process fail. Without either
variable, ordinary local tests deliberately skip MariaDB; a package-level PASS
therefore does not prove both engines ran. For acceptance, capture `go test -json`
and verify the relevant `/mariadb` or `_MariaDB` cases have `pass` events, not
`skip` events. Also confirm the selected test names ran: a malformed `-run`
expression can return success with `[no tests to run]`.


The script starts two sharded app processes, two HTTP monitor targets and local
webhook receivers. It verifies ownership, retry promotion, initial delivery,
escalation, acknowledgement cancellation, throttle persistence across process
restart, and recovery resolution. It stops its processes on exit and prints the
directory containing logs and `report.json`. Ports default to 38766–38768; use
`--port` to choose a different consecutive block. Use a fresh database for each
run. This is local runtime coverage, not a remote probe or browser UI acceptance
test; populated deployment migration rehearsal remains a release gate.

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

Match CI instead of drifting: CI pins `v2.12.2`
(`.github/workflows/ci.yml`, `golangci/lint-action`). Install that exact version
when you want local results to agree with the PR gate.

**If it panics rather than linting.** A build of `golangci-lint` older than the Go
stdlib it is asked to analyse crashes with a type-checker error inside the standard
library (e.g. `could not import math/rand/v2 ... method must have no type
parameters`) followed by a `go/types` stack dump and exit 2. That is a toolchain
mismatch, not a code defect, and it is easy to misread as "the linter is broken
here, use the `go vet` fallback". Align the toolchain with `go.mod` (currently
`go 1.26.6`) instead:

```bash
GOTOOLCHAIN=go1.26.6 golangci-lint run
```

The same pin the MariaDB contract suites in §2 use. `go vet` and `gofmt -l`
are unaffected by this and remain the fallback only when the binary is genuinely
absent.
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

# Template (MariaDB mode, in-release server + persistence)
helm template uptime-phoenix charts/uptime-phoenix \
  --set database.engine=mariadb \
  --set mariadb.enabled=true

# Template (the checked-in in-release MariaDB overlay)
helm template uptime-phoenix charts/uptime-phoenix \
  -f charts/uptime-phoenix/values-internal-mariadb.yaml

# Render matrix + assertions (what gate-full and CI run).
# This is more than "does it render": it fails if mariadb.enabled=true does not
# produce a StatefulSet/PVC/NetworkPolicy, if the DSN is not wired to the
# in-release Service, if the MariaDB Pod carries the labels the all-in-one
# Service selects on, or if the ambiguous database configs stop failing loudly.
make helm-validate
```

### 7.1 Live MariaDB check (kind / minikube / any cluster with a StorageClass)

`helm template` cannot see the two failure modes that mattered here: a generated
credential that differs between the Secret and the ConfigMap, and a client binary
that does not exist in the image the chart pins. Deploy it:

```bash
minikube start --driver=docker
kubectl create namespace phoenix-mariadb
helm upgrade --install e2e ./charts/uptime-phoenix -n phoenix-mariadb \
  --set database.engine=mariadb --set mariadb.enabled=true --set ingress.enabled=false \
  --set mariadb.persistence.size=1Gi --set mariadb.resources.requests.memory=256Mi

# 1. Both Pods Ready (the app Pod only becomes Ready if /api/health/ready, which
#    pings the database, passes).
kubectl -n phoenix-mariadb get pods,pvc

# 2. Migrations really ran on the PVC-backed server.
kubectl -n phoenix-mariadb logs deploy/e2e-uptime-phoenix | grep -c 'Applying migration'   # 34

# 3. The initContainer gated on an authenticated connection, not on TCP.
kubectl -n phoenix-mariadb logs deploy/e2e-uptime-phoenix -c wait-for-mariadb

# 4. Persistence: delete the database Pod and confirm the schema survives the
#    rescheduled Pod's re-attach of the same claim.
kubectl -n phoenix-mariadb delete pod e2e-uptime-phoenix-mariadb-0
kubectl -n phoenix-mariadb wait --for=condition=Ready pod/e2e-uptime-phoenix-mariadb-0

# 5. Chart smoke tests (API health + authenticated DB probe).
helm test e2e -n phoenix-mariadb --logs

# 6. Credential retention: an upgrade must not change the generated values.
helm upgrade e2e ./charts/uptime-phoenix -n phoenix-mariadb --reuse-values
kubectl -n phoenix-mariadb get secret e2e-uptime-phoenix -o go-template \
  '{{ len .data.mariadb-root-password }}'   # still 44 (32 chars, base64)
```

**Verify:**
- No template rendering errors
- All YAML is valid
- ConfigMap has correct env vars
- `mariadb.enabled=true` renders `statefulset-mariadb.yaml`,
  `service-mariadb.yaml` and the MariaDB PVC, and `db-dsn` targets
  `<release>-mariadb:3306` (until 2026-09 this flag rendered a PVC and a DSN for
  a server that nothing deployed — `make helm-validate` now asserts it)
- The MariaDB Pod does **not** carry `app.kubernetes.io/name: uptime-phoenix`
  alone: the all-in-one Service selects on name+instance, so a colliding label
  would send Phoenix HTTP traffic to the database
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
- [ ] Dashboard leaves skeleton cards once monitors exist (must not hang on "No monitors" after WS connect; `monitor.list` must not be drop-on-full)

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
- [ ] Leave "Include acknowledgement link" off (the default), trigger a DOWN, and confirm no ack URL/button is sent; turn it on and confirm Discord gets an Acknowledge button while other providers append `Acknowledge: …`
- [ ] On a monitor's detail page, leave "Include target" on (default) and confirm its alert carries the URL/host; turn it off for one monitor sharing a channel and confirm its alert omits the target while the sibling monitor still includes it
- [ ] On a Discord webhook, add extra Link buttons (label + URL or `{{ ack_url }}`) and confirm they appear under the embed
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

S3 health checks (`head_bucket` / `head_object` / `get_object`) must test
effects, not only status codes:

- `Validate` accepts hyphen and underscore bucket names; `_` cannot be used
  with `path_style=false`.
- A 200 signed probe is UP; 403 / 404 / connect errors / 301 redirects are DOWN.
- Underscore buckets send path-style (`/bucket` on the endpoint host), never
  `bucket.endpoint` as the Host header.
- `Authorization` is SigV4 (`AWS4-HMAC-SHA256`). Secrets never appear in
  `Message`. Redirects are not followed.
- `tls_ignore` against a self-signed target is UP; verification on is DOWN.
- No usage/quota config keys (`check_storage`) and no full-bucket listing.

Database capacity conditions (`check_session_pool` / `check_storage`) must test
effects, not only messages/status codes. `storage_scope=instance` must use the
instance-wide fixed query (not `current_database()` / `DATABASE()`) and report
condition scope `instance` for PostgreSQL/MySQL:

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


### Local delivery cutover regression gate (2026-09-20)

`TestLocalDeliveryContract` composes the real heartbeat, alert, throttle and outbox
repositories with the dispatcher/consumer and a recording provider. It exercises
both SQLite and MariaDB when `TEST_MARIADB_DSN` is supplied. Keep the normal
bootstrap dispatcher enabled until the full M1 cutover acceptance work in
`docs/multi-region/IMPLEMENTATION_STATUS.md` is complete.

```bash
go test -race -count=1 ./internal/adapters/repository -run 'TestLocalDeliveryContract|TestProbeActivationLocksSourceThroughCommit|TestProbeInstallationRacesSnapshotWriter'
```

This gate asserts actual sends and their captured settings, maintenance continuity,
reminders, acknowledgement before a first send, recovery after acknowledgement,
durable summary retries, applied revision changes during an outage, immutable
channel selection, existing-045 lease preservation, and the 050 downgrade guard.
The activation test attempts writes on a second connection after the source read
and before receipt commit; a pre-activation edit alone does not prove this property.

Migration 050 requires stopping **all** API/worker writers on MariaDB while copying
and atomically replacing the outbox table. It preserves delivery IDs and leases.
Do not edit an already-applied migration to change an existing installation.
A populated attempt-zero cancellation intentionally blocks downgrade to 049.

Run `scripts/multi_region_smoke.py` with the disposable database setup above to
exercise bootstrap, two sharded workers, actual webhook calls, step-zero escalation,
acknowledgement, process restart and recovery. Its fixed five-second scheduler-rate
assertion can be timing-sensitive; record an initial failure and any fresh-database
retry separately, rather than describing a retry as an uninterrupted pass.

## M3 source stream-reset checkpoint

Run `rtk proxy go test -race -count=1 ./internal/adapters/repository/edge ./internal/adapters/probe ./internal/adapters/auth -run 'TestEdgeStreamReset|TestStreamReset'`.
These cases use real SQLite, protected certificate material and TLS/WebSocket
connections. They cover WAL-only evidence, authenticated archives, sync/backup/
late-transaction failure, exact retry, capacity, guarded downgrade, epoch chains,
certificate resealing and restart with unchanged pin/token/bootstrap files.
The publication retry regression keeps directory sync failing after an archive
is already visible; reset must preserve the original live epoch. Tests do not
claim to simulate an actual machine power cut. See
[source reset evidence](multi-region/M3_SOURCE_RESET_ACCEPTANCE.md) and the hub/CLI
acceptance below; the older compiled replay/certificate run was only a source
checkpoint regression check.

## M3 hub stream reset and operator recovery

Run `rtk proxy go test -race -count=1 ./internal/adapters/repository ./internal/adapters/probe ./cmd/probe -run 'TestProbeStreamReset|TestStreamReset|TestProbeResetCLI'`.
Set the documented `TEST_MARIADB_DSN` to a disposable live engine and verify its
named cases ran; a skipped engine is not acceptance. Tests cover stale parent/
child leases, both transaction rollback boundaries, history/ciphertext retention,
UNKNOWN current state, exact retries, closed metadata, unresolved rotations and
populated downgrade refusal. The source CLI exercises a pending archive failure
and lost post-commit output through real startup.

Build the ordinary app, probe and phoenix-probe-admin binaries, then run
`scripts/probe_runtime_smoke.py` with `--verify-replay --verify-stream-reset` and
`--mariadb-container` under the existing disposable `_smoke` DB setup. Do not
combine the reset flag with rotation flags; their unresolved overlap intentionally
blocks reset. This process test exercises both recoverable restart orderings,
metadata-only receipts, archive integrity, UNKNOWN before peer confirmation,
actual authenticated completion and independent sequence one in old/new streams.
Diagnostic SQLite readers must close explicitly; read a published archive with
`mode=ro&immutable=1` so verification cannot create sidecars. See
[hub reset acceptance](multi-region/M3_HUB_RESET_ACCEPTANCE.md).

## M3 bounded shutdown and pressure

Run `rtk proxy go test -race -count=1 ./cmd/probe ./internal/adapters/probe ./internal/adapters/scheduler ./internal/core/services ./internal/adapters/notifier -run 'EdgeHealthPressure|EdgeRuntime_RealTLS_ReplayBatchAndACK|EdgeRuntimeQuiescence|EdgeDrainRejects|EdgeSchedulerAndProviderQuiescence|SourceDeliveryQuiescence|SMTP'`.
The real SQLite/TLS cases test withheld versus committed ACKs, rejected new
sessions, joined management callbacks and a fixed source prefix. Real HTTP checks
and provider calls must finish and commit during quiescence, or honor cancellation
when their grace ends. A canceled check must not invent a DOWN observation.
Pressure crosses the exact 80% threshold; declared gaps clear only after ACK.
SMTP cases cover cancellation before a greeting, during DATA receipt and before
connecting, plus existing MIME/recipient/template behavior and TLS refusal.

Add `--verify-shutdown` to the compiled `--verify-replay` process smoke. Each edge
termination must finish within 25 seconds; the report captures actual latency,
durable target/cursor, flushed versus retained state and exact remaining bytes.
Both healthy flush and offline retention must occur. Completed in-flight checks
may legitimately advance the high-water mark during shutdown; compare retained
bytes and monotonic progress rather than assuming no new final result commits.
See [shutdown acceptance](multi-region/M3_SHUTDOWN_ACCEPTANCE.md). This does not
prove metadata cleanup or the complete fifteen-minute M3 partition.

## M3 source metadata and physical storage bounds

Run `rtk proxy go test -race -count=1 -v ./internal/adapters/repository/edge -run 'TestEdgeMetadata|TestEdgeStorage'`.
Use real encrypted payloads to exhaust the separate 64 MiB metadata quota. Assert
the selected revision and counter roll back, old eligible history frees capacity,
active references and pending/leased deliveries survive, and queued telemetry
stays byte-for-byte unchanged. Exercise populated migration 010 down/up and refusal
after old config retirement, including an existing oversized store.

The physical tests log actual DB/WAL sizes over repeated cycles. An independent
SQLite reader pins a snapshot across writes: once the WAL admission threshold is
reached, `CheckWritable` and new writes must fail without growing the file, then
recover after the reader releases. The 16 MiB admission threshold allows one
transaction's additional frames; `journal_size_limit` alone is not a hard quota.
See [storage acceptance](multi-region/M3_STORAGE_BOUNDS_ACCEPTANCE.md).

## M3 complete partition process acceptance

Use fresh app/probe/admin binaries and a new disposable `_smoke` MariaDB schema
with `scripts/probe_runtime_smoke.py`. Combine `--verify-replay --verify-history
--verify-watchdog --verify-command --verify-partition --verify-shutdown
--command-partition-seconds 900`, plus the existing binary/output/container flags.
The transparent relay interrupts the management link while retaining end-to-end
TLS. A second target fails during the partition, survives an edge restart and
recovers before reconnection. The first target keeps its original pending ACK.

The report must show at least 900 measured monotonic seconds, exact retained
payload digests and original observation microseconds, one receipt per source
sequence, no hub sends for mirrored edge incidents, and initial fresh state applied
before newly received history beyond the pre-reconnect hub cursor. Legitimate hub
watchdog sends are identified by the durable hub incident-ownership table. A lost
ACK may leave already-received events on the edge; their earlier receipt times
must remain unchanged. The private `partition-prefix.json` preserves input digests
and timestamps before ACK pruning. The original ACK must have one
source receipt and one accepted transition, and cannot silence a later incident.
Check every enabled verification flag and process exit, not only a printed PASS.
A 30-second rehearsal is useful but its `milestone_duration_met` is false.

Keep the workers active during assignment setup. The real MariaDB
`TestProbeAssignmentReplacementUsesConfigurationLockOrder` regression covers the
publication/replacement deadlock discovered by this scenario. Do not hide that
failure with an unconditional retry in the harness or by stopping workers.

## M3 ordered replay integration

`TestProbeReplayAcceptance` in `internal/adapters/repository` runs the same mixed
replay/receipt contract on SQLite and MariaDB when `TEST_MARIADB_DSN` points to a
disposable test database. It covers stale and expired leases, exact retained
configuration and channel authority, historical/future state separation, rejected
and duplicate prefixes, concurrent ingestion, timestamp precision, final-write
rollback and guarded migration 054 downgrade. Run with `-race -count=1`.

The private edge `TestReplay*` tests cover byte preservation, bounds, holes,
exhaustion, ACK fences, both rollback directions and reopen. Probe adapter
`TestReplayAcceptance*`, `TestEdgeReplay*` and
`TestEdgeRuntime_RealTLS_ReplayBatchAndACK` exercise ACK/retry loops and real TLS
lost-ACK reconnect through the production path. Local socket access is required.

For full processes, build `cmd/app`, `cmd/probe`, `cmd/phoenix-probe-admin` with
`CGO_ENABLED=0` and use `scripts/probe_runtime_smoke.py --verify-replay` with its
three binary paths, a new private output directory, and `--mariadb-container`.
`DB_DSN` must name a fresh localhost database ending in `_smoke`. This checks two
hub workers, offline DOWN/provider retry, edge restart, UP recovery, exact backlog
mirrors and durable cursors, zero hub send intents, then a second cold restart.
It stops its child processes. See [replay acceptance](multi-region/M3_REPLAY_ACCEPTANCE.md).

## M3 historical recomputation

`TestProbeHistory*` in `internal/adapters/repository` exercises the transactional
history worker, bounded gap/backward-clock range expansion, parent dependencies,
last-write rollback, restart, assignment boundaries, legacy statistics and
on-demand read consistency. Supply `TEST_MARIADB_DSN` to run the real MariaDB
cases, including the deterministic source/parent transaction barriers. SQLite
separately verifies serialization through independent connections. Use `-json`
and inspect actual engine pass/skip events; a package PASS or an unmatched `-run`
expression is not proof of execution.

The core `TestProbeHistory*` tests exercise gap/freshness/sequence rules, per-region
sample weighting and independent overall policy. Migration 057 guards downgrade
when duration coverage or pending repair ranges would be lost. Migration fixture
restore lists must include 057; a 037 SQLite table rebuild removes later columns.

Add `--verify-history` to the process command above (it requires `--verify-replay`).
It waits for a closed minute and checks source sample count, persisted duration,
complete overall coverage and consumed work through real restarted hub workers.
This smoke is shorter than the final required M3 15-minute partition acceptance.
See [history acceptance](multi-region/M3_HISTORY_ACCEPTANCE.md) and its linked
retrospective for exact validation evidence and remaining milestone work.

## M3 runtime ownership and watchdog timer

`TestProbeRuntime*` and `TestProbeConnectorLeaseContract` in the shared repository
package cover both engines with `TEST_MARIADB_DSN`: competing owners, same-worker
duplicate loops, reconnect retention, stale release/renewal, child expiry bounds,
backward-clock deadlines, rollback, disable, legacy adoption and takeover during
replay. Migration 058 refuses to discard even released owner epochs. Keep fixture
migration restore lists current when rebuilding prior schemas.

`TestProbeConnectorRuntime*`, `TestProbeConnectorEnrollment*` and
`TestProbeWatchdog*` in core services cover lifecycle cancellation, enrollment
while a worker already owns retries, monotonic loss/recovery timing and measured
handoff checkpoints. A timer unit test is not watchdog notification acceptance.

The runtime process smoke with `--verify-replay` now starts both workers before
operator enrollment and proves that an edge restart advances connection generation
without changing runtime ownership. It continues the offline delivery/replay and
history assertions above. See [runtime acceptance](multi-region/M3_RUNTIME_ACCEPTANCE.md)
and [the retrospective](postmortems/2026-09-21-m3-integration.md#assignment-and-authority). Both watchdogs,
commands/offline ACK and the fifteen-minute partition remain separate M3 gates.

## M3 edge watchdog source transaction

Run `go test -race -count=1 ./internal/core/domain ./internal/adapters/repository/edge -run 'Test(Edge|Probe)Watchdog'`
for exact lifecycle validation and the real edge SQLite transaction. These cases
cover competing commits, stale config/session/version, counter and final-write
rollback, restart with ACK, backward source wall clocks, duplicate delivery IDs,
disable/re-enable, migration rollback and existing lease/telemetry preservation.
`TestProbeWatchdogRecoveryCannotInventAcknowledgement` and
`TestProbeWatchdogRecoveryCheckpointCannotPageAgain` are regressions for rejected
source effects, not just status-code checks.

This gate supplements the full Go race suite with `TEST_MARIADB_DSN` set to a
disposable test DB, CGO-free build and lint. See
[source acceptance](multi-region/M3_WATCHDOG_SOURCE_ACCEPTANCE.md) and its evidence.
It does not by itself prove a running watchdog. Exercise the integrated hub
ownership/replay, health callbacks and provider reconciliation through the enabled
both-side process acceptance above.

## M3 hub watchdog source transaction

Run `go test -race -count=1 ./internal/adapters/repository -run '^TestHubWatchdog'`
with `TEST_MARIADB_DSN` set to a disposable local database. Verify actual
MariaDB-named pass events and no engine skips. The contract covers source and
mirror ownership, runtime/session/config fences, late rollback, competing writers,
restart/ACK, exact timestamp precision, typed delivery collisions and DB-clock
expiry during encoding. Migration tests preserve availability leases across
SQLite rollback and MariaDB interrupted copy/atomic rename.

Legacy outbox migration tests must restore 059 after reconstructing 045/050/051;
otherwise the shared MariaDB schema no longer matches `_migrations`. See
[hub watchdog acceptance](multi-region/M3_HUB_WATCHDOG_ACCEPTANCE.md) for the full
gate and its checkpoint-specific config/runtime/provider boundaries. Storage tests do not
constitute both-side watchdog process acceptance.

### M3 independent incoming health

Run `go test -race -count=1 ./internal/adapters/probe` for the real WebSocket/TLS
regressions. `TestSessionHealth*` covers blocked callbacks, original monotonic
receipt time, unhealthy sample order, overload/join, duplicate Run and stale/wrong
role rejection. `TestHubHealthConfirmsConfigImmediatelyAfterDurableReceipt` must
observe fresh health authority while its config receipt callback remains blocked,
and must still withhold the applied revision until commit. This is transport
acceptance; the complete watchdog runtime/provider and M3 partition checks remain.


### M3 saved watchdog settings and real operator command

Run `go test -race -count=1 ./internal/adapters/repository -run '^TestProbeWatchdogSettingsContract$'`
with `TEST_MARIADB_DSN` naming a disposable local DB. Both engine branches must
execute: complete watchdog-only dependencies, zero-assignment snapshots,
concurrent CAS, restart/no-op, disabled registration reads, missing references,
late link-write rollback, notification deletion and migration constraints.
The legacy 035 boundary fixture must downgrade/reapply 060 before its parent.

Run `go test -race -count=1 ./cmd/phoenix-probe-admin` for the actual command in
separate processes over a fresh SQLite DB. It checks single-JSON stdout during
initial migrations, durable settings across restart, explicit enable selection,
revision conflict, numeric overflow and no invented applied receipt. Migration
diagnostics belong to slog/stderr, not the command's result stream.

`TestConfigProbeMetadataUsesExactBoundedFields` rejects null, missing, overlong
and case-alias metadata; `TestProbeWatchdogCustomTimingPreservesPositiveWireIntervals`
keeps the V1 positive timing range with a lower suspect threshold only when a
custom loss interval is 45 seconds or shorter. These prerequisites do not prove
running watchdogs, provider delivery, commands or full partition recovery.

## M3 certificate source storage

Run `go test -race -count=1 ./internal/adapters/auth ./internal/adapters/probe ./internal/adapters/repository/edge -run 'TestEdgeCertificate|TestEdgeCredentialRotation|TestRuntimeIdentity'`.
These tests use actual generated certificates, authenticated encryption and private
SQLite databases. They assert effect/receipt atomicity, reopened-store recovery,
metadata/ciphertext validation, no plaintext persisted private keys, version/pin
matching, session fences, overlap expiry/clock rollback, mutual exclusion with
credential rotation, capacity and active-material preservation. Populated migration
checks preserve an existing ACK and reject destructive certificate downgrade.

The full live `TEST_MARIADB_DSN` gate also exercises strict existing hub receipt
validation: certificate-only details must not be accepted on credential commands.
This is a storage foundation; passing it does not prove live TLS switching,
bootstrap-expiry recovery or hub certificate rotation. See
[certificate storage acceptance](multi-region/M3_CERTIFICATE_STORAGE_ACCEPTANCE.md)
and [subsequent acceptance](multi-region/M3_HUB_CERTIFICATE_ACCEPTANCE.md).

## M3 hub credential rotation

Run `go test -race -count=2 ./internal/adapters/repository -run 'TestProbeCredentialRotation|TestProbeCommandStorage|TestProbeRegistryContract'`
with `TEST_MARIADB_DSN` naming a disposable database, and verify actual named
MariaDB passes with no engine skips. The repeated run checks that migration
rehearsals leave the shared schema intact. Rotation cases assert protected
issuance, candidate authentication without activation, rollback, fences,
capabilities, retained dependencies and lost activation receipt recovery after
the original deadline.

Run the real transport cases with
`go test -race -count=1 ./internal/adapters/probe ./internal/core/services -run 'TestHubCredentialRuntime|TestCredentialRuntime|TestProbeConnectorCredential|TestCommandRuntime'`.
They require local sockets and test both-store restart, lost receipts, unchanged
ACK behavior, source quiescence and bounded forced closure. A successful result
write is not evidence that a hub callback committed before cancellation.

For independent processes, use the existing harness with `--verify-replay
--verify-credential-rotation`, all three binary paths, a fresh output directory
and a new disposable localhost `_smoke` MariaDB schema. It tests operator-issued
rotation through two workers, offline restarts, source receipts, cold reauthentication
and unchanged telemetry identity. The complete M3 fifteen-minute partition is a
separate acceptance requirement. See
[hub rotation acceptance](multi-region/M3_HUB_CREDENTIAL_ACCEPTANCE.md).


## M3 certificate source TLS runtime

Run `go test -race -count=1 ./internal/adapters/probe ./internal/adapters/repository/edge -run 'TestCertificateRuntime|TestCertificateCommandCodec|TestCommandCodecs|TestEdgeCertificateAdmission|TestEdgeTLSRecovery|TestCredentialRuntime'`.
The fixture uses `http.Server.ServeTLS` with the production manager, rather than
an httptest server that may supply a static fallback certificate. Pinned clients
verify no candidate before activation, new selection after commit, lost-result
recovery after cold restart and overlap retirement, and rejection of a delayed
old handshake without advancing the durable generation. Tests cover admission
expiry, first-preparation and late-activation socket lifetimes, historical receipt
grace, concurrent cache recovery, failed reconciliation, TLS resumption disabled,
expired bootstrap recovery, and corrupt/wrong-key/expired active material.

Standalone codec tests retain optional top-level fields while rejecting duplicate
keys and invalid UTF-8, and bind the digest to original wire bytes. Full backend
race verification still requires the disposable MariaDB DSN to prove existing hub
behavior did not regress. The compiled replay/credential process harness exercises
the new TLS server. Hub certificate rotation has its separate acceptance below.
See [source TLS acceptance](multi-region/M3_CERTIFICATE_RUNTIME_ACCEPTANCE.md).


## M3 hub certificate rotation

Run `TestProbeCertificateRotation` with `TEST_MARIADB_DSN` pointing to a disposable
live MariaDB; a skipped engine is not evidence. The suite covers immutable issue/
receipt identity, atomic pin promotion and token resealing, late write rollback,
strict details, candidate selection/confirmation fencing, UTC+7 expiry round trips,
credential exclusion in both directions, retained dependencies, irreversible
retirement, pending capacity reservation, and guarded migration 063 up/down with
existing ACK/credential state preserved.

`TestProbeConnectorCertificateFallback` verifies typed pre-HTTP mismatch only,
fresh selection before fallback, unchanged credential encryption scope and fixed
deadline. `TestHubCertificateRuntimeLostResultAcrossBothStoresRestartAfterRetirement`
uses actual TLS, source and hub databases, loses the activation result, reopens
both stores and recovers after the original deadline. It initializes the immutable
command near the end of its allowed window; no persisted deadline is rewritten.

For compiled processes, run the existing smoke harness with `--verify-replay
--verify-certificate-rotation`, all three binary paths, a new private output path,
and a disposable MariaDB schema ending `_smoke`. Do not combine it with credential
rotation, which would violate the real overlap exclusion. It checks the actual CLI,
offline issuance/restart, activation receipts, promoted-pin restart and ordered
telemetry continuity. This is separate from the complete fifteen-minute M3 gate.
See [acceptance](multi-region/M3_HUB_CERTIFICATE_ACCEPTANCE.md).

## M4 Docker binding acceptance

Run `TestProbeResourceBindingContract` on SQLite and a disposable MariaDB schema
with `TEST_MARIADB_DSN` configured, and inspect the named MariaDB pass (a skip
is not acceptance). The contract covers revisioned atomic replacement, concurrent
publication, retained/tombstoned bindings and populated migration 065 down/up.
Probe tests `TestResourceBindingsRejectInvalidFiles`,
`TestDockerResourceBindingExecution`, `TestDockerRemoteEncodingStripsHubEndpoint`,
`TestHandshakeRequiresResourceBindings`, and the Docker subtest of
`TestEdgeConfigActivationRetainsExactBytesAndColdValidation` cover transport and
local resource boundaries.

For compiled verification, add `--verify-docker --verify-replay` to the existing
`scripts/probe_runtime_smoke.py` invocation with fresh app/probe/admin binaries,
a fresh disposable `_smoke` schema and `--mariadb-container`. The report must show
`docker_verified: true`, healthy bound Docker telemetry at the hub, successful
source restarts, and exact offline replay without hub provider sends. All targets
are local fixtures. See [operator details](multi-region/M4_DOCKER_BINDINGS.md).


## M4 remote TLS evidence acceptance

Run the focused source, protocol and storage tests with the documented disposable
`TEST_MARIADB_DSN` (including `parseTime=true&loc=UTC&multiStatements=true`):

```sh
rtk proxy go test -race -count=1 ./internal/core/services ./internal/adapters/probe ./internal/adapters/repository/edge ./internal/adapters/repository -run 'TestEdgeRecordingTLSMetadata|TestTLSObservationReplayAndSnapshotWire|TestEdgeTLSCheckSurvivesRestartAndPruning|TestProbeTLSEvidenceAcceptance'
```

Audit the exact `TestProbeTLSEvidenceAcceptance/mariadb` result and every child;
a skipped MariaDB parent is not acceptance. Cases cover expiry precision, source
HTTPS execution, source restart/pruning, duplicate replay, current snapshots ahead
of history, explicit null/omission, immutable same-sequence TLS, stale authority,
historical assignment isolation, concurrent snapshot/replay transactions, late
rollback and populated migration 066 down/up with evidence-preserving refusal.

For compiled-process acceptance, add `--verify-tls --verify-replay` to
`scripts/probe_runtime_smoke.py` with fresh app/probe/admin binaries, a fresh
private output directory, a fresh disposable `_smoke` schema and the existing
MariaDB container flag. This makes the main HTTP target a local HTTPS fixture and
compares source TLS bytes with the hub's history, current state and `tls_info`
projection across offline checks, process restarts and ACK pruning. It can be
combined with `--verify-docker`. The fixture's per-monitor `tls_ignore` setting
does not alter probe-management pin validation or production trust defaults.
See [the slice acceptance](multi-region/M4_TLS_EVIDENCE.md) for executed evidence
and the remaining certificate-alert/capacity work.

## M4 certificate paging acceptance

Run the focused source, protocol and storage tests with the documented disposable
`TEST_MARIADB_DSN` (including `parseTime=true&loc=UTC&multiStatements=true`):

```sh
rtk proxy go test -race -count=1 -timeout 2400s ./internal/core/services ./internal/adapters/probe ./internal/adapters/repository/edge ./internal/adapters/repository -run 'TestCertAlertPaging|TestEdgeCertificatePaging|TestCertificateIncident|TestProbeCertificatePagingAcceptance'
```

Audit the exact `TestProbeCertificatePagingAcceptance/mariadb` result and every
child; a skipped MariaDB parent is not acceptance. Coverage splits as follows.

Pure evaluator (`TestCertAlertPaging*`, `internal/core/services`): the 30/14/7
matrix, one alert per threshold per certificate, advance retiring then re-firing in
order, renewal opening a new identity, recovery retiring without provider work,
maintenance consuming neither a threshold nor an incident, a looser reading of the
same certificate leaving the tighter alert alone, active/inactive channel fan-out,
an accepted graph missing a named channel failing closed, and every invalid context
(missing clock/ID source, foreign cursor, non-certificate open incident, unbounded
issuer) rejected with `domain.ErrValidation` and no side effects. Assert the
suppressed paths allocate zero UUIDs — a silent identity is a silent duplicate.

Durable source lifecycle (`TestEdgeCertificatePaging*`, real recorder + encoder +
edge SQLite): incident row identity, cursor pointing at exactly that incident, the
observation-then-transition event order, the delivery row's immutable snapshot, raw
checker metadata never reaching storage, a same-threshold recheck paging nothing
before **and after a process close/reopen**, threshold advance emitting two ordered
transitions, one open incident per identity (the partial unique index), a stale
`ExpectedCertificateVersion` returning `ErrStaleLocalState`, a malformed subject
rolling back alerts, events, provider work and the stream sequence unchanged, and
the 011 rebuild preserving rows while a populated downgrade is refused by the guard.

End-to-end provider effect (`TestEdgeCertificatePagingSendsOnceAcrossRestart`): a
real self-signed HTTPS target with a five-day certificate, the production HTTP
checker, the real store and the real delivery worker. Assert exactly one provider
call carrying `certificate_expiry` with the right threshold/days/issuer/expiry and
regional scope; that a transient provider failure retries the **same** intent rather
than paging twice; that the outcome becomes `delivery.result` telemetry; and that
further checks across two more restarts never reopen a delivered threshold.

Wire contract (`TestCertificateIncident*`, `internal/adapters/probe`): encode →
persisted bytes → hub-side `decodeReplayBatch` mapping preserves the exact subject
(including a fractional expiry second); malformed subjects (unfixed threshold, null
expiry, threshold on availability/watchdog, capacity, escalation, aggregate scope)
are refused by the encoder and by the decoder.

Storage bounds (`TestProbeCertificatePagingAcceptance`, SQLite **and** MariaDB):
mirror lifecycle with zero `probe_delivery_intents`, immutable subject enforced on
restatement, acknowledgement refused, orphan delivery refused, duplicate receipt
adding no rows, and populated migration 067 down/up with the evidence guard.

No compiled-process acceptance gate exists for this slice yet: `--verify-cert-paging`
is not implemented in `scripts/probe_runtime_smoke.py`, and the TLS slice's
`--verify-tls` fixture certificate is not inside the paging window. Treat the
Go-level matrix above as the current evidence and read
[the acceptance record](multi-region/M4_CERT_PAGING.md) for what stays unverified.

## M4 escalation acceptance

Run the source, wire, and edge storage tests:

```sh
rtk proxy go test -count=1 -timeout 180s ./internal/core/domain ./internal/core/services ./internal/adapters/probe ./internal/adapters/repository/edge ./internal/adapters/repository -run 'TestAvailabilityEscalation|TestEdgeRecordingArmsAndCancelsEscalation|TestEdgeEscalationSurvivesRestart|TestAvailabilityEscalationRoundTrips|TestEdgeConfigDecoderAcceptsEnabledEscalation|TestRemoteConfigEncoderPublishesEnabledEscalation|TestAccessReplayAvailabilityEscalation|TestIncidentEscalationRoundTrip'
```

The edge case must reopen SQLite, advance one due step, and refuse migration
`014` down while that ladder exists. A skipped or missing case is not acceptance.
There is no `probe_runtime_smoke.py` flag for this slice. See
[the acceptance record](multi-region/M4_ESCALATION.md).

## M0–M4 review regressions

Use a disposable `TEST_MARIADB_DSN`; these repository tests delete fixture data.
Run one MariaDB suite at a time per database. The gate requires both engines and
fails if any expected case was skipped or did not execute:

```sh
: "${TEST_MARIADB_DSN:?Set a disposable MariaDB test DSN}"
export TEST_MARIADB_DSN
rtk proxy go test -race -count=1 -timeout 2400s -json ./internal/core/services ./internal/adapters/repository ./internal/adapters/repository/edge -run '^(TestBackupRestoreKeepsMonitorInactiveUntilAssignmentsCommit|TestBackupRestoreSchedulerAdmission|TestHistoryClearSurvivesStreamReset|TestRegionalRecoveryResolvesPublicIncident|TestRegionalRecoveryRequiresFreshOverallUp|TestEdgeEscalationPreservesEarlierDelivery)$' > /tmp/m04-review.jsonl
rtk proxy python3 - <<'PY'
import json
from pathlib import Path

events = [json.loads(line) for line in Path('/tmp/m04-review.jsonl').read_text().splitlines()]
passed = {event.get('Test') for event in events if event['Action'] == 'pass'}
required = {
    'TestBackupRestoreKeepsMonitorInactiveUntilAssignmentsCommit',
    'TestRegionalRecoveryRequiresFreshOverallUp',
    'TestEdgeEscalationPreservesEarlierDelivery',
}
for engine in ('sqlite', 'mariadb'):
    required.add(f'TestBackupRestoreSchedulerAdmission/{engine}')
    for variant in ('new_fence', 'upgraded_fence'):
        required.add(f'TestHistoryClearSurvivesStreamReset/{engine}/{variant}')
    for path in ('replay', 'snapshot'):
        required.add(f'TestRegionalRecoveryResolvesPublicIncident/{engine}/{path}')
bad = [event for event in events if event['Action'] in ('fail', 'skip')]
assert not bad, f'Failed or skipped results: {bad}'
assert required <= passed, f'Missing required results: {sorted(required - passed)}'
print('All M0–M4 review regressions passed, including SQLite and MariaDB.')
PY
```

See [the fix and evidence record](multi-region/M4_REVIEW_FIXES.md) for the exact
effects tested and migration 070's downgrade guard. The full backend race gate,
CGO-free build and linter remain required for changes to these paths.

## M5 read API foundation

M5 is complete; [completion evidence](multi-region/M5_COMPLETION.md) extends the
earlier scoped health and desired-assignment slices. See the
[contract and continuation guide](multi-region/M5_FOUNDATION.md) before extending
it; null runtime diagnostics are deliberate and do not imply application.
The [fleet diagnostics slice](multi-region/M5_FLEET_DIAGNOSTICS.md) adds the
admin `GET /api/probes` list/detail reads and the safe diagnostic read port
that now feeds the formerly-null connection/config fields. The
[assignment write slice](multi-region/M5_ASSIGNMENT_WRITES.md) adds the admin
`PUT /api/monitors/:id/probes` replacement, atomic create-with-assignments and
the clone rule.

Set `TEST_MARIADB_DSN` to a disposable MariaDB database with
`parseTime=true&loc=UTC&multiStatements=true`. Run one engine suite at a time per
database. The JSON audit below is mandatory: supplying a DSN alone does not prove
that MariaDB tests executed.

```sh
: "${TEST_MARIADB_DSN:?Set a disposable MariaDB test DSN}"
export TEST_MARIADB_DSN
rtk proxy go test -race -count=1 -timeout 2400s -json ./internal/core/services ./internal/adapters/http/handlers ./internal/adapters/repository ./internal/adapters/ws ./internal/adapters/notifier -run 'TestMonitorRegional|TestRegionalDisplayHealthBoundaries|TestM5|TestProbeFleet|TestProbeDiagnostics|TestProbeAdmin|TestValidateDesired|TestProbeAssignmentService|TestMonitorServiceClone|TestValidEnrollment|TestRuntimeEndpoint|TestPublicRegionalCoverage|TestRegionalNotificationAttribution|TestRegionalEventsRecheck|TestRegionalCommandEvent|TestAccessChangePurges' > /tmp/m5-read.jsonl
rtk proxy python3 scripts/m5_read_evidence.py /tmp/m5-read.jsonl
```

The audit also accepts a full `go test -race -count=1 -timeout 60m -json ./...`
log. It requires successful completion of the affected packages and named SQLite
and MariaDB cases, rejects failed tests and rejects required skips/missing passes.
The suite now includes source revocation/deletion, ACK requester authority,
current browser audience, public overall coverage and notification attribution.
Full backend build, race tests, lint, vulnerability checks, frontend type/unit/
build/lint and browser gates remain applicable. Run frontend generation/check,
unit tests, build and E2E sequentially: the browser harness embeds `web/dist`,
and Paraglide generation replaces files under the unit-test search root.

### M5 real enrollment and partition acceptance

`scripts/m5_runtime_smoke.py` starts a real hub, autonomous edge, pinned-TLS
partition relay, local checker target and webhook sink. Use a **fresh disposable**
localhost MariaDB database whose name ends in `_smoke`; the script refuses other
DSNs. Set `DB_DSN` with `parseTime=true&loc=UTC&multiStatements=true`, then:

```sh
rtk proxy env CGO_ENABLED=0 go build -o /tmp/m5-app ./cmd/app
rtk proxy env CGO_ENABLED=0 go build -o /tmp/m5-probe ./cmd/probe
rtk proxy python3 scripts/m5_runtime_smoke.py --app-binary /tmp/m5-app --probe-binary /tmp/m5-probe --output /tmp/m5-runtime-acceptance
```

Use an unused output directory. It contains private runtime identities and logs;
only its redacted `report.json` is suitable for a committed evidence record.
The harness exercises API registration/enrollment from initialized source IDs,
two regional streams, offline pending configuration and ACK, stale UNKNOWN,
reconnect/application receipts, and one original remote outage incident/delivery.
It terminates its own processes in `finally`. This process test complements
browser and scoped-authority tests; it does not establish M6 load, populated
production migration, kill-point or deployment acceptance.

## M6 fleet assignment-ownership gate (T34)

Guards the mixed-version rollout: a desired set containing any member other than
`local` is refused with `409 worker_fleet_unaware` while a live hub worker cannot
be shown to enforce assignment ownership. Design, detection boundary and
acceptance are in
[multi-region/M6_FLEET_ACTIVATION_GATE.md](multi-region/M6_FLEET_ACTIVATION_GATE.md).

```sh
# Service gate, scheduler attestation ordering, and the store on SQLite.
GOTOOLCHAIN=go1.26.6 go test -race -count=1 \
  -run 'TestFleetActivationGate|TestProbeAssignmentServiceReplaceFleetGate|TestMonitorServiceCreateWithAssignmentsFleetGate' \
  ./internal/core/services/
GOTOOLCHAIN=go1.26.6 go test -race -count=1 -run 'TestShardedScheduler' ./internal/adapters/scheduler/
GOTOOLCHAIN=go1.26.6 go test -count=1 -run 'TestHubWorkerReadiness' ./internal/adapters/repository/

# The same store cases against real MariaDB. REQUIRED: without the DSN every
# mariadb subtest silently SKIPS and the run still prints "ok".
TEST_MARIADB_DSN='phoenix:phoenix@tcp(127.0.0.1:43316)/phoenix_ci?parseTime=true&loc=UTC&multiStatements=true' \
  GOTOOLCHAIN=go1.26.6 go test -count=1 -run 'TestHubWorkerReadiness' ./internal/adapters/repository/
```

Confirm the MariaDB legs actually ran before trusting the result — count named
passes with `go test -json` and require `skips=0`:

```sh
TEST_MARIADB_DSN=... GOTOOLCHAIN=go1.26.6 go test -count=1 -json \
  -run 'TestHubWorkerReadiness' ./internal/adapters/repository/ \
  | python3 -c 'import sys,json;p=s=f=0
for l in sys.stdin:
    l=l.strip()
    if not l.startswith("{"): continue
    e=json.loads(l); a,t=e.get("Action"),e.get("Test")
    if not t: continue
    p,s,f = (p+1,s,f) if a=="pass" else (p,s+1,f) if a=="skip" else (p,s,f+1) if a=="fail" else (p,s,f)
print(f"passes={p} skips={s} fails={f}")'
```

This case is engine-sensitive, not just engine-parity: SQLite's driver normalizes
a local-zoned `time.Time` on write while MariaDB stores the local wall clock, so
`TestHubWorkerReadinessAttestationIsUtcBound` only discriminates a missing
`.UTC()` on the MariaDB leg. A SQLite-only run of that test proves nothing about
rule 6.

Regression areas if you touch this gate: `scheduler.ShardedScheduler` claim and
refresh ordering, `MonitorRepo.ClaimBatch`'s `LocalHubExecutionSQL` predicate,
both remote-write entry points (`ProbeAssignmentService.Replace`,
`MonitorService.CreateWithAssignments`, which `Clone` also uses), and the
`Restore` exemption — a backup import or config apply must still succeed on a
degraded or mid-rollout fleet.

## M6 operator requirements

Egress, durability, clock, disk, backup and provider bounds are recorded in
[multi-region/M6_OPERATOR_REQUIREMENTS.md](multi-region/M6_OPERATOR_REQUIREMENTS.md).
That record is not a load envelope, a populated migration rehearsal, or a canary.
The admission numbers it cites are frozen by
`TestOperatorStorageBudgetsMatchDocumentedContract`:

```sh
GOTOOLCHAIN=go1.26.6 go test -count=1 -run 'TestOperatorStorageBudgetsMatchDocumentedContract' \
  ./internal/adapters/repository/edge/
```

Changing `walCheckpointBytes`, the page-cap formula, the 64 MiB delivery or
metadata budgets, or the 1 GiB stream-reset archive cap requires updating the
requirements document in the same change. The chart NetworkPolicy still does
not open the default probe port; `helm template` with `networkPolicy.enabled=true`
must not grow an 8443 egress rule unless the requirements document changes too.
