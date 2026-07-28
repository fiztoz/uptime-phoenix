# Phoenix — Roadmap

> 5-phase delivery. Each phase produces a deployable, demoable increment. The domain code never rewrites between phases — only adapters change.

> **CI removed 2026-07-25 (`9de75e9`) — not coming back.** Every "CI" reference below that
> predates this date is a historical record of what was true when it was written; it is not
> current. Phoenix is now **locally-gated**: `make gate-full` (see `docs/TESTING.md`) is the
> whole gate, run by hand before every merge. Forward-looking items below that assumed a
> future CI pipeline have been reworded to describe the local-gate equivalent.

---

## Current Status (2026-07-27)

| Phase | Status | Last Major Update | Notes |
|-------|--------|-------------------|-------|
| **Phase 0** | ✅ Complete | 2026-06-10 | Scaffold, hexagonal structure, CI, Dockerfile, Helm chart scaffold |
| **Phase 1 S1** | ✅ Complete | 2026-06-15 | Domain, ports, MariaDB/SQLite migrations (22 tables), auth (JWT + TOTP) |
| **Phase 1 S2** | ✅ Complete | 2026-06-18 | All 12 current checkers implemented + tested, scheduler, WS hub, heartbeat service |
| **Phase 1 S3** | ✅ Complete | 2026-06-20 | Frontend (Svelte 5), embedded static assets, Helm values, `docker compose up` working |
| **Phase 2 S4–S6** | ✅ Complete | 2026-06-23 | 11 notification providers, status pages, incidents, tags, maintenance windows, Prometheus `/metrics`, Playwright E2E scaffold |
| **Docker / Dev Stack** | ✅ Fixed | 2026-06-24 | `docker compose up` now works with zero manual setup (env var defaults, no `.secrets/`) |
| **Phase 3** | ✅ Complete | 2026-06-24 | `cmd/api` + `cmd/worker` + shared `internal/bootstrap`, Redis EventBus, MODE gating, RequestID/security headers/API rate limit, optional OTLP, MariaDB backup/partition CronJobs (Helm), Sprint 8 hardening |
| **Phase 4 S9** | ✅ Complete | 2026-06-25 | AggregateService with real rollups, notification settings UI, public status page API, domain resolution, heartbeat history API, testing instructions |
| **Phase 4 S10** | ✅ Complete | 2026-06-27 | i18n (en+th), sparklines, heartbeat API, favicon badge, mobile responsive pass, rollup CronJob |
| **Phase 4 S11** | ✅ Complete | 2026-07-25 | Sharded worker + Helm leases shipped; Sprint D removed the O(monitors²) WS stats path, reached 84 ms event p95 at 1k and 405 ms at 10k split mode, and completed the rolling-deploy soak |
| **Phase 4 S12** | ✅ Complete | 2026-06-28 | 3 new notifiers (Bark/Feishu/Line), public dark mode, subscriber model, WebAuthn/passkey login, Cloudflare Tunnel chart support, Grafana datasource + dashboards |
| **Notification dispatch** | ✅ Complete | 2026-06-30 | Auto-alerting wired: retry-confirm (PENDING→DOWN after `max_retries`), dispatcher fires on confirmed status change + recovery, honors `resend_interval`, suppressed during maintenance windows |
| **User management + scoped RBAC** | ✅ Complete | 2026-07-18 | Closed registration, admin user API, recursive monitor/group grants, capability flags, shallow grants, and create capabilities (`9c5473b`, `4b4fc94`, `347d7b9`) |
| **Monitor groups + folder alerting** | ✅ Complete | 2026-07-13 | First-class nested groups, derived group health, notification attachments, compare-and-set alert transitions, and reachable grant UI (`8f6c0a7`, `297d4a6`) |
| **Config backup / restore** | ✅ Complete | 2026-07-11 | Admin-only versioned JSON export/import with merge-only ID remapping and relationship restoration (`f94c78e`) |
| **Monitor proxies + status badges** | ✅ Complete | 2026-07-11 | HTTP/HTTPS/SOCKS5 proxy routing and embeddable SVG status badges (`0d65d77`, `ffa4a2b`) |
| **Status-page presentation + health** | ✅ Complete | 2026-07-17 | Full/pills/grid layouts, normalized dashboard styles, verified protected access, and operational/degraded/down health (`dcf2707`, `a530e7b`, `083d7b9`) |
| **Premium UI redesign** | ✅ Complete | 2026-07-14 | Dark design-system overhaul, responsive settings, premium controls, vector branding, and light-palette refinement (`d4ae125`, `6b80c11`) |
| **Monitor form + WebAuthn repairs** | ✅ Complete | 2026-07-18 | HTTP URL normalization, custom color/datetime controls, and persisted WebAuthn authenticator flags/options (`4220ea0`, `8ac7011`) |
| **Sprint A — Safety net** | ✅ Complete | 2026-07-19 | Restored CI/README/gate, real `tls_ignore`, secure WS+CORS defaults, build version, complete test glob, and main-surface i18n sweep (`51a4b92`, `9a7eeb7`, `1470173`, `c03088e`, `1af588f`, `7f655bd`) |
| **Sprint B — Assurance** | ⚠️ Headline complete | 2026-07-19 | MariaDB repo matrix + CI, all checker tests, zero Svelte warnings, a11y/state/mobile fixes, and five real Playwright flows; load target measured but failed (`6b17bc6`…`50b959b`) |
| **Sprint C — Visible wins** | ✅ Headline complete | 2026-07-19 | Cert-expiry alerts + maintenance IANA TZ (013); email subscriptions + OG meta + public cert (014); Kuma importer (SQLite **and** MariaDB v2) + release dry-run; see `docs/PROJECT-REVIEW-AND-ROADMAP.md` §2.3 |
| **Sprint D — Scale & trust** | ✅ Complete | 2026-07-25 | Closed R3.6 scale validation, seven notifier-adapter tests, the effect audit, MIT licensing, rolling-deploy validation, and subscription fan-out error propagation (`982b500`) |
| **F2.2 — Alert lifecycle** | ✅ Complete | 2026-07-26 | `firing → acked → resolved`, authenticated API/UI ack, public token deep-link ack, recovery resolution, acked resend suppression (`b6f1498`), handler/RBAC secrecy coverage, real-MariaDB migration-015 coverage, both browser ack paths, and heartbeat-to-dispatch effect coverage |
| **F2.3 — Escalation policies** | ✅ Complete | 2026-07-26 | Persisted ordered ladders (migration 016), nearest-wins monitor/folder assignment, DB compare-and-set lease so sharded workers cannot double-send, restart recovery, ack/resolve cancellation, policy CRUD + assignment UI, and a browser flow proving no step fires after an ack. Contracts and their test names: `docs/F2.3-ESCALATION-CONTRACTS.md` |
| **F3.2 — Incident timeline updates** | ✅ Complete | 2026-07-26 | Ordered `investigating → identified → monitoring → resolved` updates (migration 017), markdown content on public/admin timelines, create/resolve/auto-resolve seed timeline rows, and per-update subscriber email fan-out |
| **F3.3 — Uptime history page** | ✅ Complete | 2026-07-26 | Public monthly/quarterly uptime table per component from UTC daily rollups, unknown periods remain null, optional persisted SLA target (migration 018), protected-page access flow, and Playwright coverage |
| **Repository gate-debt sprint** | ✅ Complete | 2026-07-27 | 158→0 Go lint findings (shadow fixes, `bun.In`→`bun.List` migration, errcheck, misspell, unparam, prealloc, unused, unconvert, ineffassign, copyloopvar, whitespace, nilerr), 47→0 frontend formatting failures (prettier + eslint baseline), govulncheck 15→0 reachable stdlib vulnerabilities (Go 1.25.12 toolchain bump), and Makefile toolchain pinning |
| **Phase 5 S13 — OIDC SSO** | ✅ Complete | 2026-07-27 | Opt-in OIDC Authorization Code login (`coreos/go-oidc`), `(issuer, subject)` identity links (migration `019`), JIT + verified-email link policy, IdP group → admin/capability/scoped-grant mapping, HMAC state for multi-pod callbacks, local break-glass preserved, login SSO button, Helm values/Secrets. Contracts: `docs/F5-S13-OIDC-CONTRACTS.md` |
| **Phase 5 S14 — Config-as-code** | ✅ Complete | 2026-07-27 | Versioned YAML (`phoenix.dev/v1`) with stable keys (migration `020`), validate/plan/apply, idempotent second apply, optional prune of keyed resources only, secret redaction (`__REDACTED__`) with preserve-on-omit, example document, admin API. Contracts: `docs/F5-S14-CONFIG-AS-CODE-CONTRACTS.md` |
| **MariaDB 019–020 contract** | ✅ Complete | 2026-07-27 | Throwaway MariaDB 11: full `repository/...` race suite green; oidc_identities + config_keys uniques/FK/cascade verified. Report: `docs/MARIADB-019-020-CONTRACT-REPORT.md` |
| **NotificationHandlers tests** | ✅ Complete | 2026-07-27 | Effect-focused CRUD/attach/detach/test/RBAC coverage in `notification_test.go` (coverage hole closed) |
| **F3.6 — RSS/Atom + iCal** | ✅ Complete | 2026-07-27 | Public `feed.xml` (incidents Atom) + `calendar.ics` (maintenance); fail-closed access; public page links. Contracts: `docs/F3.6-FEEDS.md` |
| **F3.5 — White-label polish** | ✅ Complete | 2026-07-27 | Logo + favicon (URL or client data-URL upload), `show_powered_by` toggle, public header/footer honor branding; migration `021`. Contracts: `docs/F3.5-WHITE-LABEL-CONTRACTS.md` |

**Next Focus:** owner decisions (`Monitor.Weight`, Thai review, **first publish**). F3.4 ACME
remains on hold. **2026-07-28:** GitHub CI + release workflows restored; OIDC PKCE S256;
`phoenix-config` CLI; reverse escalation assignment UI; MariaDB 021 assured. Create GitHub
Environment `release` (required reviewers) before first publish.

---

## Phase 0 — Foundation (Weeks 1–2)

**Goal:** repo scaffold, CI, local dev environment, empty app boots.

### Deliverables
- [ ] Go module initialized (`go.mod`, `cmd/app/main.go`)
- [ ] Hexagonal folder structure: `internal/core/{domain,ports,services}`, `internal/adapters/{http,ws,repository,checker,notifier,auth,metrics,logger,scheduler}`
- [ ] SvelteKit project initialized (`web/`, Svelte 5, Vite, adapter-node)
- [ ] `Dockerfile` (multi-stage, `CGO_ENABLED=0`, distroless) + `web/Dockerfile` (node build → nginx)
- [ ] `docker-compose.yml` for local dev (app + postgres + web)
- [x] ~~`.github/workflows/ci.yml` — lint + test + build~~ — built in Sprint A
      (`51a4b92`), then deliberately removed 2026-07-25 (`9de75e9`). Superseded by
      `make gate-full`, run locally by hand.
- [ ] `golangci-lint` + `eslint` + `prettier` configured
- [ ] `makefile` with `make dev`, `make build`, `make test`, `make lint`
- [ ] Health endpoints: `GET /api/health/live`, `GET /api/health/ready`
- [ ] Structured logging via `log/slog` (JSON to stdout)

### Exit Criteria
- `make dev` starts the backend on :3000 and frontend on :5173
- `curl localhost:3000/api/health/live` returns 200
- ~~CI passes on a trivial commit~~ — moot: there is no CI (removed 2026-07-25,
  `9de75e9`). `make gate-full` passes locally instead.

---

## Phase 1 — Core Monitoring (Weeks 3–8)

**Goal:** Uptime-Kuma-equivalent monitoring engine. Single pod, all 12 monitor types, real-time dashboard.

### Sprint 1 (Weeks 3–4): Domain + Storage + Auth

- [ ] **Domain model** — `Monitor`, `Heartbeat`, `Status`, `User`, `Notification`, `StatusPage`, `Tag`, `MaintenanceWindow`, `APIKey`, `Incident` (in `internal/core/domain/`)
- [ ] **Port interfaces** — `MonitorRepository`, `HeartbeatRepository`, `EventBus`, `Checker`, `NotificationSender`, `Authenticator`, `Scheduler`, `Clock`, `Logger`, `MetricsExporter`, `ConfigProvider` (in `internal/core/ports/`)
- [ ] **MariaDB schema** — migration `001_init.up.sql` with all tables (see ARCHITECTURE.md §6). SQLite-compatible variant for dev. MariaDB partitioning by RANGE on `time` (monthly partitions).
- [ ] **Bun query builder setup** — `internal/adapters/repository/sqlite/` and `internal/adapters/repository/mariadb/`, both implementing the same repository interfaces via Bun
- [ ] **Repository adapters** — SQLite (dev/edge, via `modernc.org/sqlite`, CGO-free) + MariaDB (default K8s, via `go-sql-driver/mysql`)
- [ ] **Migrations runner** — `bun/migrate` embedded in binary; versioned `.sql` files compatible with both MariaDB and SQLite
- [ ] **Auth service + adapters** — bcrypt password, JWT issuance (`golang-jwt/jwt/v5`), TOTP (`pquerna/otp`)
- [ ] **User registration + login + 2FA setup** — HTTP handlers via Echo
- [ ] **Migrations runner** — `golang-migrate` embedded in binary

### Sprint 2 (Weeks 5–6): Monitor Engine + Real-time

- [ ] **Checker interface + registry** — `internal/core/ports/checker.go`; auto-registration via `init()`
- [ ] **Monitor type adapters** (one file each, `internal/adapters/checker/`):
  - `http.go` — status code / keyword / JSON query + TLS cert extraction
  - `tcp.go`
  - `ping.go` — `prometheus-community/pro-bing`
  - `dns.go` — `miekg/dns`
  - `websocket.go` — `coder/websocket`
  - `push.go` — inbound HTTP receiver + HMAC verification
  - `docker.go` — `docker/docker/client`
  - `mqtt.go` — `eclipse/paho.mqtt.golang`
  - `rabbitmq.go` — `github.com/rabbitmq/amqp091-go`
  - `grpc.go` — `google.golang.org/grpc` health v1
  - `snmp.go` — `gosnmp/gosnmp`
  - `database.go` — Postgres (`pgx`), MySQL (`go-sql-driver`), MongoDB (`mongo-driver/v2`), Redis (`go-redis/v9`)
- [ ] **Scheduler adapter** — in-process goroutine scheduler (`robfig/cron/v3`); ticks every monitor by interval
- [ ] **Heartbeat service** — records heartbeats, publishes to EventBus, evaluates status transitions
- [ ] **EventBus adapter** — in-process pub/sub (`adapters/eventbus/memory.go`)
- [ ] **WebSocket hub** — `coder/websocket` connection manager; subscribes to EventBus; pushes `heartbeat`, `monitor.update`, `status.change` events to authenticated clients
- [ ] **Monitor CRUD HTTP API** — `POST /api/monitors`, `GET /api/monitors`, `PUT /api/monitors/:id`, `DELETE /api/monitors/:id`

### Sprint 3 (Weeks 7–8): Frontend Dashboard + Deploy

- [ ] **Svelte 5 runes WebSocket store** — `web/src/lib/stores/ws.svelte.ts` with reconnection + typed events
- [ ] **Dashboard page** — monitor list with status pills, latency, uptime %; updates in real-time
- [ ] **Monitor create/edit forms** — `sveltekit-superforms` + `zod`; per-type config forms
- [ ] **Settings page** — app config, user profile, 2FA setup
- [ ] **Login page** — JWT auth, 2FA challenge
- [ ] **i18n** — paraglide-js initialized; English + 1 additional locale
- [ ] **UI primitives** — shadcn-svelte (Button, Dialog, Card, Input, Table, Badge), lucide-svelte icons, svelte-sonner toasts
- [ ] **Charts** — LayerCake line chart for ping/latency; uptime bar for 24h/30d
- [ ] **First Helm chart** — `charts/phoenix/` with single `phoenix` Deployment (Go binary with embedded frontend), MariaDB StatefulSet (or PVC for SQLite mode), Service, Ingress, Secret, ConfigMap, PDB. Default mode: single pod + MariaDB on PVC. `helm install` with zero external dependencies works out of the box.
- [ ] **`helm install phoenix ./charts/phoenix`** works on a local kind/k3s cluster with zero external dependencies (single pod + MariaDB PVC)

### Exit Criteria (Phase 1 = MVP)
- All 12 monitor types configurable and producing heartbeats
- Real-time dashboard updates via WebSocket
- `helm install` brings up a working tool in <2 minutes
- Single pod handles 1,000 monitors at <512 MB RAM
- Unit test coverage >70% on `internal/core/`

---

## Phase 2 — Notifications + Status Pages (Weeks 9–14)

**Goal:** full notification provider set + public status pages with custom domains.

### Sprint 4 (Weeks 9–10): Notification Engine

- [ ] **NotificationSender interface + registry** — `internal/core/ports/notifier.go`
- [x] **11 notification provider adapters** (one file each, `internal/adapters/notifier/`):
  - `telegram.go`, `discord.go`, `slack.go`, `smtp.go`, `webhook.go`, `teams.go`, `mattermost.go`, `gotify.go`, `bark.go`, `feishu.go`, `line.go`
- [x] **Notification dispatcher service** — fires on confirmed status change + recovery; respects `resend_interval`; suppressed during maintenance windows. `internal/core/services/notification_dispatcher.go`, invoked from the heartbeat path in the monitor's owning worker (exactly-once under sharded/HA). _Throttle state is in-memory (resets on restart); shared HTTP rate-limit middleware still pending._
- [ ] **Severity mapping** — UP/DOWN/PENDING/MAINTENANCE → each provider's level field (see ARCHITECTURE.md §5.3)
- [ ] **Rate-limit middleware** — shared HTTP retry with `Retry-After` header parsing; exponential backoff
- [ ] **Notification CRUD UI** — configure providers, test-send button, assign to monitors

### Sprint 5 (Weeks 11–12): Status Pages

- [ ] **StatusPage service** — CRUD, custom domain resolution, incident management
- [ ] **SvelteKit status page route** — `(public)/[domain]/+page.svelte` with `prerender = true`; SSG at build time + runtime hydration for live data
- [ ] **Custom domain routing** — `hooks.server.ts` resolves hostname → status page; CNAME table for aliases
- [ ] **Incident UI** — create/edit/resolve incidents; pinned notices; style (warning/danger/info/success)
- [ ] **Status page public API** — `GET /api/status/:slug` (no auth, cached 30s)
- [ ] **Aggregate rollup jobs** — cron job computes `heartbeat_1m` from raw heartbeats every minute; `heartbeat_1h` every hour; `heartbeat_1d` every day
- [ ] **Uptime bar chart** — 90-day uptime bar (LayerCake) on status page + detail page

### Sprint 6 (Weeks 13–14): Tags, Maintenance, Metrics, Polish

- [ ] **Tags** — create, assign to monitors (key-value), filter dashboard by tag
- [ ] **Maintenance windows** — single + cron strategies; suppress notifications during maintenance; visual indicator on dashboard
- [ ] **API keys** — generate, revoke, scope (read/write/metrics); `GET /metrics` behind API-key Basic Auth
- [ ] **Prometheus exporter** — `prometheus/client_golang`; exposes `phoenix_monitor_status`, `phoenix_monitor_latency_ms`, `phoenix_heartbeats_total`
- [ ] **Light/dark theme** — CSS custom properties; toggle persisted in localStorage
- [ ] **Favicon badge** — red dot when any monitor is DOWN (via `favico.js` equivalent)
- [x] **E2E tests** — Playwright: login, create monitor with advanced fields, see a heartbeat, configure/test-send a notification, render a public status page, and verify scoped RBAC (`46f5044`)
- [ ] **Helm chart hardening** — HPA on CPU + custom WS-connections metric; resource limits; security context (runAsNonRoot, readOnlyRootFilesystem). Chart supports toggling between embedded-frontend (default, single pod) and split-frontend (opt-in, separate `phoenix-web` Deployment).

### Exit Criteria (Phase 2 = Feature-complete)
- All 12 notification providers fire on status transitions
- Public status pages render at custom domains with live data
- `/metrics` endpoint serves Prometheus format behind API-key auth
- E2E test suite covers the full user journey
- Helm chart includes HPA, PDB, resource limits, security context

---

## Phase 3 — K8s Split + Scale Prep (Weeks 15–18)

**Goal:** separate worker from API; add Redis pub/sub for cross-pod events. Domain code unchanged.

### Sprint 7 (Weeks 15–16): Worker/API Split

- [x] **Binary split** — `cmd/api/main.go` (HTTP + WS, stateless) + `cmd/worker/main.go` (scheduler + checkers + notification dispatcher); shared `internal/bootstrap`
- [x] **Redis EventBus adapter** — `adapters/eventbus/redis.go` (pub/sub via `go-redis/v9`); selected when `REDIS_URL` env is set
- [x] **Helm chart updated** — `phoenix-api` (replicas 2-N, HPA) + `phoenix-worker` (replicas 1, PDB) + Redis opt-in. Default mode stays single-pod; split via `mode=api|worker|all`
- [x] **Rolling deploy test** ✅ 2026-07-25 (Sprint D Track C) — upgraded `phoenix-api`
      while `phoenix-worker` kept running; clients reconnect and resume without heartbeat
      loss. Manual soak — there is no CI to automate this in. Two trials: a realistic fast
      restart reconnected in ~1s (dominated by the client's own backoff, not server
      downtime — real outage was ~16ms); a ~21s stress-case outage still produced zero
      gaps in the heartbeat log. See `docs/RUNBOOK.md` §4.1 for the reproducible procedure
      and full measured results.
- [x] **Graceful shutdown** — SIGTERM handler stops scheduler, shuts down HTTP server (30s), closes EventBus

### Sprint 8 (Weeks 17–18): Observability + Hardening

- [x] **Structured logging** — `log/slog` JSON; request IDs via Echo middleware (`X-Request-ID` + context for slog)
- [x] **Distributed tracing** — OpenTelemetry SDK bootstrap when `OTEL_EXPORTER_OTLP_ENDPOINT` is set (OTLP HTTP exporter)
- [x] **Backup CronJob** — `mysqldump` to PVC nightly (Helm `cronJobs.backup`, MariaDB only)
- [x] **Partition management CronJob** — monthly `ALTER TABLE heartbeats ADD/DROP PARTITION` (Helm `cronJobs.partition`)
- [x] **Rate limiting** — per-IP / per-user API rate limit via Echo middleware (in-memory; Redis window when `REDIS_URL` set)
- [x] **Security review** — configurable CORS, CSP/security headers middleware, Bun-parameterized SQL (no raw SQL in services)
- [ ] **Load test acceptance** — the Sprint B harness measured 100 and 1,000 monitors in all-in-one mode; API p95 was 27.64 ms at 1k, but WebSocket event p95 reached 60 s. The 10,000-monitor / 100-client / <1s p99 target remains open; see `docs/LOADTEST.md` (`50b959b`).

### Exit Criteria (Phase 3 = Production-ready)
- API pods scale horizontally with zero WS session loss
- Worker restarts without missing a check interval
- Traces visible in Jaeger/Tempo
- Load test passes at 10k monitors

---

## Phase 4 — Ecosystem + Polish (Weeks 19+)

Ongoing, prioritized by user feedback. Each sprint is 2 weeks.

### Sprint 9 (Weeks 19–20): Functional Gap Fill

**Goal:** Make every backend feature reachable from the UI. No new backend work except aggregate rollups.

- [x] **Implement AggregateService** — `Rollup1m`, `Rollup1h`, `Rollup1d`, `GetUptimePercent` with real SQL queries against `heartbeat_*` tables (`aggregate_service.go`, `repository/mariadb/` + `sqlite/`)
- [x] **Wire aggregate rollup into scheduler** — cron ticks every 1m/1h/1d calling the aggregate service (`bootstrap.go`, scheduler adapter)
- [x] **Notification settings page** — create `/(admin)/notifications/+page.svelte` using existing `NotificationForm.svelte`; add test-send button
- [x] **Complete public status page** — `GET /api/status/:slug` returns monitors + uptime + incidents; frontend page fetches live data
- [x] **Wire hooks.server.ts** — implemented `resolveDomainToSlug()` calling `GET /api/status/resolve?domain=...`
- [x] **Playwright E2E: notifications + status page** — real binary-backed flows configure and test-send a provider, render a public status page, and cover the broader monitor/RBAC journey (`46f5044`)
- [x] **Verify backend tests pass** — `go test ./...` green (190 tests / 24 packages as of 2026-06-28); `go vet ./...` clean. `golangci-lint` not yet wired into CI gate.

**Exit criteria:** Every backend feature has a working UI route. Uptime % shows real data. Public status pages work.

### Sprint 10 (Weeks 21–22): UX Polish + i18n

**Goal:** Production-quality UI with theme support, internationalization, and dashboard improvements.

- [x] **Dark/light theme toggle** — store already wired in sidebar with Sun/Moon toggle, localStorage persistence, and CSS class on `<html>`
- [x] **i18n message files** — created `messages/en.json` (80+ keys) + `messages/th.json` (Thai); wired in admin layout, dashboard, monitor detail, login via `m.messageName()`
- [x] **Dashboard uptime sparkline** — created `Sparkline.svelte` (pure SVG, no external dep); integrated into MonitorCard showing 24h heartbeat latency
- [x] **Monitor detail: heartbeat timeline** — real heartbeat data from API (24h history), latency sparkline replaces hardcoded placeholder
- [x] **Favicon badge** — `favicon-badge.ts` utility overlays red dot on favicon when any monitor is DOWN; wired via `$effect` in admin layout
- [x] **Mobile responsive pass** — all pages now use mobile card layouts on small screens, bottom-sheet modals, stacked forms, responsive text sizing
- [x] **Helm chart: aggregate rollup CronJob** — `cronjob-rollup.yaml` runs 1m rollups every minute, 1h at minute 0, 1d at 00:05; gated on MariaDB engine

**Exit criteria:** App has dark mode, i18n in 2 languages, mobile-responsive pages, and sparkline charts.

### Sprint 11 (Weeks 23–24): Scale Validation + Sharded Workers

**Goal:** Validate the 10k-monitor target. Implement DB-leased sharding for multi-worker setups.

- [x] **Load test harness** — configurable k6 seeding, authenticated inventory load, real WebSocket clients, fan-out counters, and p95/p99 event-latency thresholds (`50b959b`)
- [x] **Sharded worker implementation** — `UPDATE ... WHERE` lease pattern in scheduler; workers claim monitor batches via `worker_id` + `leased_at` columns
- [x] **Worker shard Helm values** — `worker.shards.enabled` config for multiple worker replicas with automatic lease distribution; WORKER_ID from `metadata.name`
- [x] **Load test scaffold** — k6 script + reproduction README at `tests/load/`; measured results and resource snapshots in `docs/LOADTEST.md`
- [ ] **Load test pass** — 1,000 monitors / 50 WS clients was the measured functional ceiling on the Sprint B host, but event p95 was 60 s; 10k, split-mode, and <1s p99 are not validated
- [ ] **WS session resilience** — test rolling API pod restart with zero client disconnect loss

**Exit criteria:** 10k monitors pass at <1s p99. Sharded workers distribute load. Rolling deploys don't drop WS sessions.

### Sprint 12 (Weeks 25–26): Ecosystem Expansion

**Goal:** Additional providers and integrations for community adoption.

- [x] **Additional notification providers** — Signal, Matrix, Bark, Feishu, Line (each one file + registry line)
- [x] **WebAuthn/passkey 2FA** — `WebAuthnAuthenticator` port + `go-webauthn` adapter, `webauthn_credentials` migration (004), passwordless first-factor login + passkey management UI in settings; TTL-bounded single-use in-memory challenge store (env: `WEBAUTHN_RP_ID`/`WEBAUTHN_RP_NAME`/`WEBAUTHN_RP_ORIGINS`)
- [x] **Status page webhooks** — subscriber model, DB migration, repository layer for webhook notifications on incident create/resolve
- [x] **Dark mode for public status pages** — user theme toggle (Sun/Moon button) in public status page header, overrides page theme setting
- [x] **Cloudflare Tunnel built-in** — `cloudflareTunnel.enabled` Helm block: `cloudflared` Deployment (token via inline secret or `existingSecret`), NetworkPolicy egress, README + DEPLOYMENT_MODES docs
- [x] **Grafana data source plugin** — provisioned Prometheus datasource + curated `phoenix-overview` dashboard + `docker compose` quickstart under `deploy/grafana/` (built on the existing `/metrics` exposition; Prometheus-in-the-middle is the supported topology)

### Backlog (Unscheduled)
- [ ] **Persist monitor `last_status` + `down_count` instead of re-deriving them from `GetLatest`** — the alerting path currently reconstructs the previous status and the retry count by re-reading the latest heartbeat (`HeartbeatService.Record`). That read was returning a **stale row** on MariaDB until the `id` tie-break landed (2026-07-13; AGENTS.md rule 8), which could re-alert an already-alerted DOWN, miscount the retry window, or swallow a recovery. The tie-break makes the read correct, but the alerting path still depends on *sorting a table right* rather than on state it owns. Storing `last_status`/`down_count` on the monitor — exactly what `monitor_groups.last_status` now does for folders, claimed via compare-and-set — removes the class of bug by construction and closes the same multi-worker race for monitors. Needs a migration + a persisted-state path through `NotificationDispatcher`.
- [ ] Additional monitor types (RADIUS, Kafka if demand exists) — one-file plugin
- [ ] Tauri desktop wrapper (thin shell around the web build)
- [ ] Weblate integration for community translations
- [ ] Pushbullet, Alerta, Zabbix, N8N notification providers
- [ ] Multi-tenant support (multiple organizations) — on hold; Phoenix remains single-tenant
      and self-hosted unless a concrete deployment need changes that decision

---

## Phase 5 — Internal K8s Integration (Weeks 27–30) — *Focused*

> **Owner decision (2026-07-27):** Phoenix is deployed as a single-tenant application inside
> an internal K8s environment, not operated as a commercial SaaS service. Phase 5 therefore
> retains only the two capabilities that directly improve that deployment model: OIDC SSO and
> declarative config-as-code. The previous enterprise/multi-tenant scope is on hold.

### Sprint 13 (Weeks 27–28): OIDC SSO

- [x] **OIDC login** — `OIDCAuthenticator` adapter (`adapters/auth/oidc.go`) using discovery
      and Authorization Code flow; issuer, client ID, client secret, redirect URL, scopes.
- [x] **Safe identity linking/provisioning** — `oidc_identities` keyed by `(issuer, subject)`
      (migration `019`); JIT + optional verified-email link; IdP groups map to `is_admin`,
      capability flags, and `OIDC_GRANT_MAP` scoped grants. No organizations / second RBAC.
- [x] **Bootstrap and break-glass access** — local password + TOTP/passkey remain available;
      recovery and IdP-outage documented in `docs/RUNBOOK.md`.
- [x] **SSO assurance** — service + handler tests for state/nonce, callback failures, allowed
      groups, JIT, email link, grant sync, local login with OIDC on; multi-pod-safe signed
      state (no Redis). Contracts: `docs/F5-S13-OIDC-CONTRACTS.md`.

### Sprint 14 (Weeks 29–30): Declarative Config-as-Code

- [x] **Versioned YAML contract** — `phoenix.dev/v1` `Config` document with operator `key`
      fields persisted in `config_keys` (migration `020`). Relationships use keys, not DB IDs.
- [x] **Plan and idempotent apply** — `POST /api/config/{validate,plan,apply}`; second apply
      is a no-op; prune only deletes keyed resources missing from the document.
- [x] **Secret-safe GitOps** — export redacts secrets as `__REDACTED__`; apply preserves
      omitted/redacted secret fields. Runtime/OIDC credentials stay in env/Secrets.
- [x] **Operational integration** — `examples/config/phoenix-config.example.yaml` + RUNBOOK
      apply path; rollback via re-apply or `/api/backup` restore.
- [x] **Config-as-code assurance** — service tests for keys, plan, idempotent apply, prune,
      redaction, secret preserve/overwrite. Contracts: `docs/F5-S14-CONFIG-AS-CODE-CONTRACTS.md`.

### On Hold

- [ ] **F3.4 Phoenix-managed ACME/custom-domain TLS** — the internal K8s load
      balancer/Ingress owns certificate issuance, storage, and renewal. Phoenix only needs the
      correct proxy/host/WebSocket behavior and deployment documentation.
- [ ] **Enterprise/SaaS identity and isolation** — distributed WebAuthn challenge storage,
      server-side session revocation, account lockout, organizations, memberships, org roles,
      invites, tenant-scoped repositories, and org-scoped API keys.
- [ ] **Compliance/packaging work** — append-only audit UI/export, envelope encryption,
      per-tenant retention/deletion, SBOM/image signing as a product feature, a Terraform
      provider, scheduled SLO reports, and a signed Grafana plugin.

These items may be reconsidered only if a concrete deployment, threat-model, or adoption need
appears. Routine dependency updates, `govulncheck`, and the existing local security gate remain
required maintenance and are not on hold.

**Exit criteria (Phase 5 = internal-K8s ready):** operators can use the internal OIDC provider
with a tested local break-glass path; the existing single-tenant authorization model remains the
only authorization model; a versioned YAML document can be validated, dry-run, and applied
twice idempotently without leaking secrets; and Phoenix requires no application-managed TLS.

---

## Milestone Summary

| Milestone | Week | Deliverable |
|---|---|---|
| M0 — Foundation | 2 | Repo, CI, local dev, empty app boots |
| M1 — Domain + Auth | 4 | Postgres schema, repo adapters, login + 2FA |
| M2 — Monitor Engine | 6 | All 12 monitor types producing heartbeats |
| M3 — Real-time Dashboard | 8 | WebSocket-driven SPA, Helm chart, `helm install` works |
| **MVP — Phase 1 complete** | **8** | **Uptime-Kuma feature parity, single-pod K8s deploy** |
| M4 — Notifications | 10 | All 12 providers fire on status change |
| M5 — Status Pages | 12 | Public status pages at custom domains + incidents |
| M6 — Polish + Metrics | 14 | Tags, maintenance, Prometheus, E2E tests, HPA |
| **Feature-complete — Phase 2** | **14** | **All Uptime Kuma features shipped** |
| M7 — Worker/API Split | 16 | Redis pub/sub, horizontal API scaling |
| M8 — Observability | 18 | Tracing, backups, load test passes at 10k monitors |
| **Production-ready — Phase 3** | **18** | **Horizontally scalable, observable, hardened** |
| M9 — Functional Gaps | 20 | Aggregate rollups, notification UI, public status page, E2E coverage |
| M10 — UX Polish | 22 | Dark mode, i18n, sparklines, mobile responsive |
| M11 — Scale Validation | 24 | 10k-monitor load test, sharded workers, WS resilience |
| M12 — Ecosystem | 26 | Additional providers, WebAuthn, Cloudflare Tunnel, Grafana datasource |
| **Ecosystem-complete — Phase 4** | **26** | **Community-ready, scale-validated, polished** |
| M13 — OIDC SSO | 28 | Internal IdP login mapped to existing Phoenix permissions, with local break-glass access |
| M14 — Config-as-Code | 30 | Versioned secret-safe YAML with dry-run and idempotent apply |
| **Internal-K8s ready — Phase 5** | **30** | **Single-tenant, SSO-enabled, GitOps-configurable** |

---

## Effort Estimate

| Phase | Duration | Effort |
|---|---|---|
| Phase 0 | 2 weeks | 1 dev |
| Phase 1 | 6 weeks | 1–2 devs (backend + frontend) |
| Phase 2 | 6 weeks | 1–2 devs |
| Phase 3 | 4 weeks | 1–2 devs |
| **Total to production-ready** | **18 weeks** | **~1 full-stack dev or 2 specialized devs** |
