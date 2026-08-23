# Changelog

Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Released versions are git tags (`vX.Y.Z`). New work lands under `[Unreleased]`
until the next tag. Historical notes below `[0.1.0]` were written before the
first tag and stay grouped by conventional-commit type.

The project's first ~4 weeks (2026-06-23 – 2026-06-30, pre conventional-commit discipline) are
summarized rather than itemized — see [Earlier foundation](#earlier-foundation-2026-06-23--2026-06-30)
at the bottom.

## [Unreleased]

### Fixed

- Insights no longer sits on skeleton for several seconds, then ranks every
  monitor as "No qualified data", after days of UP checks. The leading-
  transition query is now `LIMIT 1` per monitor (migration `030` index
  `(monitor_id, important, time, id)`) instead of loading the full important
  history. Empty `heartbeat_1h`/`1d` rollups no longer wipe a complete
  timeline. Startup catch-up fills 26h of 1m/1h and 90d of 1d so 24h latency
  has buckets after a restart.

## [0.3.3] — 2026-08-22

### Added

- Monitors page: Collapse all / Expand all for groups (including nested
  folders). Ungrouped monitors stay visible. The unused action disables itself.

### Changed

- Dashboard no longer fetches sparkline history for every monitor on first
  paint. Each card loads its last 60 beats only when it is near the viewport,
  with a small concurrency cap. Live heartbeats append to that card only.
- `GET /api/monitors/:id/heartbeats?limit=N` now applies the cap in SQL
  (`time DESC, id DESC LIMIT N`) instead of loading the whole `hours` window
  and truncating in memory. The monitors list page also paints without waiting
  on the tag catalog.
- Scheduler runs at most 8 monitor checks at once (a restart used to fire every
  due monitor in the same second). Database checkers open one session per
  probe (`SetMaxOpenConns(1)` / `MaxConns=1`) so mysql/mariadb monitors cannot
  exhaust the target's `max_connections` (Error 1040) while the API is serving.
- Access checks cache the user row (30s) and a non-admin's resolved visibility
  set (15s, dropped immediately on grant/revoke). Concurrent users no longer
  each hit MariaDB once per monitor on the dashboard. Heartbeats stay live.

## [0.3.2] — 2026-08-20

### Fixed

- Dashboard no longer hangs on skeleton cards when WebSocket `monitor.list` is
  delayed. The hub starts read/write pumps *before* building the snapshot (so
  pings keep the socket alive and a client close is detected immediately) and
  the snapshot frame waits for a send-buffer slot instead of being dropped
  (`client send buffer full, dropped monitor.list` → broken pipe). The UI also
  hydrates from `GET /api/monitors` so first paint does not depend on that
  single frame.

## [0.3.1] — 2026-08-19

### Added

- Database storage check: `storage_scope` (`database` default, or `instance`)
  on one monitor. PostgreSQL sums every non-template database; MySQL sums
  visible schemas. Still logical database size versus `storage_max_gb`, not
  host disk. Instance-wide PostgreSQL needs `CONNECT` on each database or
  `pg_read_all_stats`.

### Fixed

- Expired or missing WebSocket JWT no longer reconnects forever. The hub
  closes unauthorized sockets with `4001` (the range the dashboard already
  treated as logout). Pre-fix servers sent `1008`, which the client retried,
  so the UI sat on pending waiting for `monitor.list`. The client now treats
  both codes as a dead session. `JWT_EXPIRE_HOURS <= 0` is rejected at
  startup and clamped in the authenticator so a bad env cannot mint a token
  that is already expired.

## [0.3.0] — 2026-08-19

### Added

- Database monitors: optional session-pool and storage checks as capacity
  **conditions** (`ok` / `warning` / `error`, plus derived `stale`). Over
  threshold or a missing grant does not flip the heartbeat to DOWN, open an
  outage, or change Insights / SLA / public status. Warning, error, and
  recovery promote after two consecutive samples with 5-point hysteresis.
  Chips, Needs attention, RBAC-scoped REST/WebSocket, and
  `capacity_condition` notifications are included. Fixed engine queries only
  (no operator SQL). Setup: `docs/guides/database-monitor-setup.md`.
- Dashboard and wallboard **Card: Capacity** view: session-pool and storage
  meters replace the ping sparkline when those signals exist; other monitors
  keep the response graph. Choice persists in `localStorage`.
- S3 / object-storage monitor (`s3`): signed `head_bucket`, `head_object`, or
  `get_object` against AWS, MinIO, and S3-compatible endpoints. Health only —
  no usage or quota probe. Bucket names may include `-` and `_`; `_` forces
  path-style addressing. Setup guide: `docs/guides/s3-monitor-setup.md`.
- Generic K8s extensions catalogue + iframe tab: Helm `extensions: []`
  (default off) renders per-item Deployment, Service, NetworkPolicy, and
  an Ingress path before `/`. Phoenix reads `PHOENIX_EXTENSIONS` (JSON
  `{id, title, path, icon}` only) and serves `GET /api/extensions`. Empty or
  unset → `[]`. Admin sidebar appends a tab that iframes the registered
  path and uses `{path}/icon.svg` from the plugin image (overridable
  `icon`, Puzzle fallback). Not a new monitor type; image, secretName, and
  credentials are never serialized.

### Fixed

- Monitor detail and dashboard no longer refetch in a loop until the API
  returns 429. `beginConditionSnapshot` reads `conditionSeq` and
  `applyConditionSnapshot` increments it; those calls are now untracked.

## [0.2.3] — 2026-08-16

### Added

- HTTP JSON assertions can accept a directly translatable JSONPath subset
  (`$`, child names, array indexes, and `[*]`) while continuing to use GJSON at
  runtime. The new `has_value` condition rejects missing or empty values without
  treating valid `false` and `0` values as empty.
- Helm: JWT signing key can be supplied as a stable `secret.jwt` value or an
  existing Secret so API/all-in-one pods do not mint a new key on every
  rollout (which logged everyone out). Helm CLI still auto-generates when
  neither is set.

### Fixed

- Pausing or resuming a monitor no longer clears its folder, proxy, owner,
  flags, retry policy, or weight. Collapsing a group no longer un-nests it
  or blanks its name. Omitted PUT fields stay as stored; explicit `null`
  still clears placement.

## [0.2.2] — 2026-08-15

### Changed

- Helm: empty `image.tag` / `web.image.tag` now default to `Chart.AppVersion`
  instead of `latest`. A chart-version bump rolls pods onto the matching
  published image. Override the tag only to pin a different image.
- Helm: the API HPA is created only when `hpa.enabled` is true.

### Added

- Helm: API, worker, and all-in-one Deployments now carry `checksum/config` and
  `checksum/secret` pod-template annotations. An Argo CD sync (or `helm upgrade`)
  that changes a chart-managed ConfigMap or Secret rolls those pods so they
  pick up the new env. The optional web and cloudflared Deployments hash their
  nginx ConfigMap and tunnel token the same way.
- Helm: API Deployment accepts extra environment variables from values.
- WebSocket hub now updates `phoenix_ws_connections_active`. The HPA Pods
  metric is a second opt-in (`hpa.wsConnections.enabled`) so CPU scaling works
  without prometheus-adapter.

## [0.2.1] — 2026-08-15

### Added

- HTTP monitors: optional JSON response assertions. Configure `json_query` (gjson path) plus `json_operator` (`exists`, `not_exists`, `equals`, `not_equals`, `contains`, `not_contains`) and `expected_value` when comparing. The monitor form groups these fields; Uptime Kuma imports that already have an expected value map to `equals`.
- Helm: `values-production-split.yaml` overlay for split API + sharded workers + in-release Valkey against an external MariaDB. The MariaDB DSN now honors `mariadbExternal.password`.

### Fixed

- Go toolchain `1.26.6` for stdlib CVEs (`GO-2026-6218` and related). `go.mod`, `GOTOOLCHAIN`, CI, and the release workflow are aligned.

## [0.2.0] — 2026-08-14

In-release Valkey EventBus (official `valkey` 0.11.0 subchart), wired to `REDIS_URL`. Chart and app version `0.2.0`. See the [v0.2.0 GitHub release](https://github.com/fiztoz/uptime-phoenix/releases/tag/v0.2.0).

## [0.1.0] — 2026-08-09

First public release. See the [v0.1.0 GitHub release](https://github.com/fiztoz/uptime-phoenix/releases/tag/v0.1.0).

## Historical notes (pre-versioned changelog)

The items below were written while the tree had no tags. They are kept for
searchability and are not a complete per-release delta.

### Added

#### Post–Sprint D / Phase 5 close-out (2026-07-27 – 2026-07-28)

- GitHub Actions **restored** (owner): `.github/workflows/ci.yml` (PR/main gate) and
  `release.yml` (dry-run + owner-gated publish). Local `make gate-full` remains required
  offline. See `docs/TESTING.md` and `docs/RELEASING.md`.
- OIDC PKCE S256 (always on when OIDC is enabled); `cmd/phoenix-config` CLI;
  reverse escalation assignment UI; MariaDB migration `021` white-label columns.
- Gate-debt sprint: Go lint / frontend format / `govulncheck` brought to a clean baseline
  under Go 1.25.12.

#### Sprint D — Scale & trust, Track C (2026-07-25)

- `LICENSE` (MIT) added at the repo root; referenced from `README.md` and
  `charts/uptime-phoenix/Chart.yaml` (`artifacthub.io/license` annotation) and
  `charts/uptime-phoenix/README.md`.
- `make gate-full`: the complete local pre-merge gate (build/vet/gofmt, `-race` tests,
  `golangci-lint`, frontend check/test/build/lint/e2e, `helm lint`/`template`,
  `govulncheck`, `git diff --check`). `make govulncheck` added standalone. (At the time
  of this track CI was absent; CI was restored 2026-07-28 and now also runs `actionlint`.)
- Rolling-deploy WebSocket soak (`docs/RUNBOOK.md` §4.1): reproducible procedure and
  measured result proving `uptime-phoenix-api` restarts lose no heartbeats while
  `uptime-phoenix-worker` keeps running, and that WS clients reconnect and resume
  automatically. Closes the Phase 3 / Sprint B carryover item.
- `Monitor.Weight` kept and **wired for display sort** (API list `ORDER BY`, UI). Earlier
  “inert field” notes are obsolete.

### Removed (historical — later reversed)

- `.github/` (CI workflows) removed 2026-07-25 (`9de75e9`) while the project was
  local-gate only. **Restored 2026-07-28** — do not treat “no CI” notes below that date
  as current. See `docs/TESTING.md`.

#### Sprint C — Visible wins (2026-07-19)

- Certificate-expiry alerts: opt-in `cert_expiry_notify`, exact TLS `NotAfter`, fixed 30/14/7 day once-per-threshold alerts with restart-safe state, maintenance suppression, and certificate-specific templates on all 11 notifiers (migration `013`).
- Maintenance IANA `timezone` for cron windows (default UTC), half-open `[start,end)` evaluation, form picker, and backup round-trip (migration `013`).
- Status-page email subscriptions: double opt-in JWT tokens, per-page active SMTP channel, public subscribe/confirm/unsubscribe, admin channel + subscriber UI, incident and maintenance fan-out mail; dormant webhook table preserved as `status_page_subscribers_legacy_webhook` (migration `014`).
- Public status page: optional `cert_expiry_date` / `cert_days_left`, `subscriptions_available`, and server-side OG/Twitter metadata injection into the embedded SPA shell.
- `PUBLIC_URL` config + Helm `config.publicUrl` for absolute links in subscription emails.
- Uptime Kuma importer (`cmd/kuma-import`): read-only converter from **SQLite or MariaDB/MySQL** (Kuma v2) to Uptime Phoenix `BackupDocument` JSON; see `docs/KUMA-IMPORT.md`.
- Release dry-run workflow (contents:read, no publish), multi-arch Dockerfile `TARGETARCH` fixes, `docs/RELEASING.md`.

- [`4220ea0`](https://github.com/fiztoz/uptime-phoenix/commit/4220ea0) Normalize HTTP monitor URLs on save; add a custom color picker and datetime picker to the admin UI.
- [`347f4cb`](https://github.com/fiztoz/uptime-phoenix/commit/347f4cb) RBAC: `can_create_monitors` / `can_create_groups` capability flags; shallow (non-recursive) group grants.
- [`a530e7b`](https://github.com/fiztoz/uptime-phoenix/commit/a530e7b) Status pages: grid dashboard style and three-level public health display.
- [`56ae31a`](https://github.com/fiztoz/uptime-phoenix/commit/56ae31a) Incident severity levels, with matching status display.
- [`6b80c11`](https://github.com/fiztoz/uptime-phoenix/commit/6b80c11) Premium dropdown redesign and multi-select filters.
- [`f192fa6`](https://github.com/fiztoz/uptime-phoenix/commit/f192fa6) SVG wordmark component, replacing text-based branding.
- [`027a15c`](https://github.com/fiztoz/uptime-phoenix/commit/027a15c) `avatar_url` support on the Discord webhook notifier.
- [`dcf2707`](https://github.com/fiztoz/uptime-phoenix/commit/dcf2707) Status pages: configurable dashboard style (full vs. pills — grid followed later).
- [`9050ceb`](https://github.com/fiztoz/uptime-phoenix/commit/9050ceb) Open Graph meta tags for Discord link previews.
- [`297d4a6`](https://github.com/fiztoz/uptime-phoenix/commit/297d4a6) Folder (monitor-group) alerting on the group's own rollup status; the RBAC grant UI actually rendered for the first time (functions existed but no markup called them); a real confirmation modal replacing `window.confirm()`.
- [`d4ae125`](https://github.com/fiztoz/uptime-phoenix/commit/d4ae125) Premium dark design system overhaul.
- [`95d7fbc`](https://github.com/fiztoz/uptime-phoenix/commit/95d7fbc) RBAC admin grant UI (backend), capability gating, atomic status-page monitor reorder.
- [`4b4fc94`](https://github.com/fiztoz/uptime-phoenix/commit/4b4fc94) RBAC: scoped monitor/group visibility, capability flags, dashboard filters, monitor tags embedded in the wire payload.
- [`db4b090`](https://github.com/fiztoz/uptime-phoenix/commit/db4b090)/[`8f6c0a7`](https://github.com/fiztoz/uptime-phoenix/commit/8f6c0a7) Monitor groups: introduced as single-level parent/child nesting, then replaced with first-class monitor-group entities (folders) the next day.
- [`f94c78e`](https://github.com/fiztoz/uptime-phoenix/commit/f94c78e) Backup: full config export/import with ID remapping.
- [`0d65d77`](https://github.com/fiztoz/uptime-phoenix/commit/0d65d77) Outbound proxy support (HTTP/HTTPS/SOCKS5), assignable per monitor.
- [`61e649b`](https://github.com/fiztoz/uptime-phoenix/commit/61e649b) Response-time chart data on the public status page.
- [`ffa4a2b`](https://github.com/fiztoz/uptime-phoenix/commit/ffa4a2b) Embeddable SVG status badges.
- [`13b75fa`](https://github.com/fiztoz/uptime-phoenix/commit/13b75fa) Status pages: password gate, custom CSS, tags, and a public chart (this commit also fixed the `PasswordHash` leak on the public endpoint — see Fixed).
- [`9c5473b`](https://github.com/fiztoz/uptime-phoenix/commit/9c5473b) Auth: disable open self-registration after the first user; admin-driven user management with RBAC.
- [`697f178`](https://github.com/fiztoz/uptime-phoenix/commit/697f178) Monitor ownership enforcement, chart API, detail-page state refinements.
- [`68cbfaa`](https://github.com/fiztoz/uptime-phoenix/commit/68cbfaa) Uptime-Kuma-style monitor detail observability, plus related auth fixes.
- [`2119fd7`](https://github.com/fiztoz/uptime-phoenix/commit/2119fd7) Monitor statistics API and UI components.
- [`8ab3041`](https://github.com/fiztoz/uptime-phoenix/commit/8ab3041) API key management and user authentication enhancements.
- [`b65c6bb`](https://github.com/fiztoz/uptime-phoenix/commit/b65c6bb) Endpoint to list notifications for a specific monitor.
- [`a03b706`](https://github.com/fiztoz/uptime-phoenix/commit/a03b706) Public push-monitor ingest endpoint with HMAC verification.

### Fixed

- [`8ac7011`](https://github.com/fiztoz/uptime-phoenix/commit/8ac7011) Persist WebAuthn authenticator flags correctly; unwrap public-key options that were double-wrapped.
- [`083d7b9`](https://github.com/fiztoz/uptime-phoenix/commit/083d7b9) Close out status-page dashboard code-review findings.
- [`fcfef6f`](https://github.com/fiztoz/uptime-phoenix/commit/fcfef6f) Normalize status-page dashboard styles server-side and return verified public access correctly.
- [`294eda5`](https://github.com/fiztoz/uptime-phoenix/commit/294eda5) Use the WebSocket monitor status as a fallback for status display when the initial fetch lags.
- [`8da6781`](https://github.com/fiztoz/uptime-phoenix/commit/8da6781) Soften the light-mode color palette for eye comfort.
- [`954fa36`](https://github.com/fiztoz/uptime-phoenix/commit/954fa36) `HeartbeatRepo.ListByMonitor` returned the wrong slice when ordered ascending — now returns the most recent beats correctly.
- [`ba083d6`](https://github.com/fiztoz/uptime-phoenix/commit/ba083d6) Mascot mark switched to a cropped vector; fixed login-page logo centering.
- [`c471b45`](https://github.com/fiztoz/uptime-phoenix/commit/c471b45) Status pages: return `409` on a duplicate slug instead of a raw `500`.
- [`2f2d551`](https://github.com/fiztoz/uptime-phoenix/commit/2f2d551) Closed a batch of silent-failure bugs: incident resolve, missing defaults, retention loop, API-key expiry.
- [`60d30b9`](https://github.com/fiztoz/uptime-phoenix/commit/60d30b9) Real uptime data on status pages, first-class maintenance status, and UTC normalization at the repository boundary (see AGENTS.md rule 6).
- [`2a1f41e`](https://github.com/fiztoz/uptime-phoenix/commit/2a1f41e) Made maintenance-window alert suppression actually suppress alerts (it previously did nothing); unblanked the response-time chart.
- [`718b7de`](https://github.com/fiztoz/uptime-phoenix/commit/718b7de) Return a snake_case `TagView` instead of the raw `domain.Tag` (wire-shape discipline).
- [`cd41470`](https://github.com/fiztoz/uptime-phoenix/commit/cd41470) Maintenance windows: scope to the authenticated user, enforce ownership, fix the wire shape.
- [`aab1375`](https://github.com/fiztoz/uptime-phoenix/commit/aab1375) Fixed MariaDB zero-timestamp insert failures (`ERROR 1292`) and backfilled the admin flag on upgrade.
- [`4873d26`](https://github.com/fiztoz/uptime-phoenix/commit/4873d26) Closed a cross-tenant authorization bypass in the monitor-ownership check.
- [`81d0873`](https://github.com/fiztoz/uptime-phoenix/commit/81d0873) API-key middleware now reads the context user ID via the `ContextUserIDKey` constant instead of a stray `"user_id"` literal.
- [`68cd43e`](https://github.com/fiztoz/uptime-phoenix/commit/68cd43e) Persist the monitor interval display correctly; bound the response-time chart range.
- [`06dc133`](https://github.com/fiztoz/uptime-phoenix/commit/06dc133) Corrected the join condition in `GetByMonitorID` for both MariaDB and SQLite.

### Changed

- [`692f928`](https://github.com/fiztoz/uptime-phoenix/commit/692f928) `Makefile`: add a `gate`/`gate-fast` target and stamp the build with a version ldflag.
- [`955f750`](https://github.com/fiztoz/uptime-phoenix/commit/955f750) gofmt drift cleanup in `internal/core` (`user.go`).
- [`fa612df`](https://github.com/fiztoz/uptime-phoenix/commit/fa612df) Badges: switch to `viewBox` for scalable, crisp SVG rendering.
- [`26b3b26`](https://github.com/fiztoz/uptime-phoenix/commit/26b3b26) Centralize monitor target extraction behind a single `Target()` method (domain/ws/notifier).
- [`7a4e077`](https://github.com/fiztoz/uptime-phoenix/commit/7a4e077) gofmt drift cleanup across Go files; frontend type-tooling fixes.
- [`90e8390`](https://github.com/fiztoz/uptime-phoenix/commit/90e8390) Fixed the dead "accepted status codes" UI path, exposed monitor advanced fields, unified heartbeat types.

### CI

- [`6568651`](https://github.com/fiztoz/uptime-phoenix/commit/6568651) Restored the GitHub Actions pipeline (`.github/workflows/ci.yml`): backend build/vet/gofmt/race-test + golangci-lint, a MariaDB-11 job that boots the binary, asserts every migration applies, and runs all three smoke suites against a freshly recreated database each, a Bun frontend job, and Docker image builds — closing the gap `c8c74be` opened.
- [`c8c74be`](https://github.com/fiztoz/uptime-phoenix/commit/c8c74be) CI workflows removed entirely. The gate and MariaDB smoke suites kept working locally but ran only when someone remembered — the exact gap `6568651` closes.

### Docs

- [`cb0ac05`](https://github.com/fiztoz/uptime-phoenix/commit/cb0ac05) Add the root `README.md` (the repo had none).
- [`4275196`](https://github.com/fiztoz/uptime-phoenix/commit/4275196) `docs/PROJECT-REVIEW-AND-ROADMAP.md`: full project review, refinement plan ("Wave R"), and forward roadmap ("Wave F").
- [`75e7560`](https://github.com/fiztoz/uptime-phoenix/commit/75e7560) Handoff doc: confirmed remaining bugs, a verification recipe, hard-won rules.
- [`04beb8d`](https://github.com/fiztoz/uptime-phoenix/commit/04beb8d) Handoff doc for the next agent: backup/export-import notes + hard-won rules.
- [`63cc05a`](https://github.com/fiztoz/uptime-phoenix/commit/63cc05a) Uptime Kuma parity audit; agent guidelines for auth, wire-shape discipline, and parallel work (the seed of `AGENTS.md`).

### Earlier foundation (2026-06-23 – 2026-06-30)

The first ~8 days, before the project settled into one-commit-per-fix conventional-commit
discipline. Commit messages here are broader and less atomic than the rest of this file, so
they are summarized rather than itemized:

- **Initial scaffold and bootstrap** — [`f567b47`](https://github.com/fiztoz/uptime-phoenix/commit/f567b47) `init`, [`bfb2080`](https://github.com/fiztoz/uptime-phoenix/commit/bfb2080) bootstrap-user functionality.
- **Phase 3** — [`7a453d6`](https://github.com/fiztoz/uptime-phoenix/commit/7a453d6) API/worker split, observability, hardening.
- **Phase 4, Sprints 9-10** — public status API + custom domains ([`1dc4824`](https://github.com/fiztoz/uptime-phoenix/commit/1dc4824)), notification settings CRUD + test-send ([`d5448cb`](https://github.com/fiztoz/uptime-phoenix/commit/d5448cb)), rollup/uptime `AggregateService` ([`270c5c4`](https://github.com/fiztoz/uptime-phoenix/commit/270c5c4)), i18n + sparklines + favicon badge + heartbeat API ([`122d9a9`](https://github.com/fiztoz/uptime-phoenix/commit/122d9a9)), LayerCake charts ([`74e6efd`](https://github.com/fiztoz/uptime-phoenix/commit/74e6efd)), responsiveness pass ([`0309082`](https://github.com/fiztoz/uptime-phoenix/commit/0309082)), general functionality/perf work ([`8e9a772`](https://github.com/fiztoz/uptime-phoenix/commit/8e9a772)), a switch to Bun for the dev/testing toolchain ([`5aa5d83`](https://github.com/fiztoz/uptime-phoenix/commit/5aa5d83)).
- **Phase 4, Sprints 10-12** — mobile UX, rollups, sharded scheduling, notifiers ([`20cde92`](https://github.com/fiztoz/uptime-phoenix/commit/20cde92)); the split deployment stack and its docs ([`b07df67`](https://github.com/fiztoz/uptime-phoenix/commit/b07df67)); the first premium dark console UI pass and split web tier ([`e410030`](https://github.com/fiztoz/uptime-phoenix/commit/e410030), [`5fdc69f`](https://github.com/fiztoz/uptime-phoenix/commit/5fdc69f)); a local Colima k8s redeploy script ([`63e3cbf`](https://github.com/fiztoz/uptime-phoenix/commit/63e3cbf)).
- **Auth, deploy, notifications groundwork** — WebAuthn passkeys, Cloudflare Tunnel, Grafana provisioning ([`3dd3f1c`](https://github.com/fiztoz/uptime-phoenix/commit/3dd3f1c)); split dev setup enhancements ([`973d0aa`](https://github.com/fiztoz/uptime-phoenix/commit/973d0aa)); test-notification endpoint ([`17d49ca`](https://github.com/fiztoz/uptime-phoenix/commit/17d49ca)); trimmed the notifier list down to the 11 supported providers ([`2a9057b`](https://github.com/fiztoz/uptime-phoenix/commit/2a9057b)); automatic alert dispatch wired to status transitions ([`d39bc67`](https://github.com/fiztoz/uptime-phoenix/commit/d39bc67)); linter-warning cleanup and typed context keys ([`4e11821`](https://github.com/fiztoz/uptime-phoenix/commit/4e11821), [`ca70b79`](https://github.com/fiztoz/uptime-phoenix/commit/ca70b79)).
- Also in this window: roadmap/testing docs tracking Phase 4 sprint status ([`8677075`](https://github.com/fiztoz/uptime-phoenix/commit/8677075), [`899424e`](https://github.com/fiztoz/uptime-phoenix/commit/899424e), [`1af4ffe`](https://github.com/fiztoz/uptime-phoenix/commit/1af4ffe), [`8324401`](https://github.com/fiztoz/uptime-phoenix/commit/8324401)).
