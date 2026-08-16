# Uptime Phoenix — Project Instructions for All Agents

> **READ THIS FIRST.** Every agent (AI or human) working on Uptime Phoenix must follow these rules.
> Uptime Phoenix is a self-hosted, K8s-native, minimal-dependency monitoring tool.
> Architecture: Port-and-Adapter (Hexagonal). Stack: Go + Svelte 5. Database: MariaDB.
>
> **This file is the canonical source of truth for project rules.** It is designed to be
> tool-agnostic and works with any AI coding agent as well as human contributors. If your
> tool auto-loads a different filename (e.g. Cursor, Copilot, Gemini CLI), that file simply
> points back here.

## Reference Documents

Before writing any code, read the relevant design doc:
- `docs/PLAN.md` — project goal, scope, success criteria, locked decisions
- `docs/ROADMAP.md` — phased delivery timeline, what to build in which sprint
- `docs/ARCHITECTURE.md` — detailed technical design (15 sections, the single source of truth)
- `docs/TESTING.md` — how to test every change (gate commands, manual checklists, regression areas)
- `research/uptime-kuma.md` — original Uptime Kuma research (feature reference)
- `research/uptime-kuma-stack-alternatives.md` — why Go + Svelte 5 was chosen
- `research/uptime-kuma-k8s-architecture.md` — K8s deployment design

## Architecture Rules (Non-Negotiable)

### 1. Hexagonal Boundaries
```
cmd/ ──▶ adapters/ ──▶ core/services/ ──▶ core/ports/ ──▶ core/domain/
           │                                              (pure types)
           └── implements core/ports/* interfaces
```

- **`internal/core/domain/`** — pure Go types. No framework imports. No DB driver imports. No HTTP imports. Only stdlib + domain types.
- **`internal/core/ports/`** — interfaces only. No implementations. No external imports.
- **`internal/core/services/`** — use cases. Depends ONLY on ports and domain. Never imports an adapter, a DB driver, or an HTTP framework.
- **`internal/adapters/`** — implementations of ports. This is where Echo, Bun, WebSocket, checker libraries, and notification HTTP calls live.
- **`cmd/`** — composition root. Wires adapters to services. The ONLY place that knows about concrete implementations.

**Violation check:** if you see `import "github.com/labstack/echo"` or `import "github.com/uptrace/bun"` inside `internal/core/`, it is a bug. Fix it.

### 2. One-File Plugin Convention
- Each monitor type = **one file** in `internal/adapters/checker/` implementing `ports.Checker`.
- Each notification provider = **one file** in `internal/adapters/notifier/` implementing `ports.NotificationSender`.
- Both auto-register via `init()` in their `registry.go`.
- Adding a new monitor type or notification provider = create one file + add one line to `registry.go`. No other changes.

### 3. EventBus is a Port
- `core/ports/eventbus.go` defines `EventBus` interface.
- `adapters/eventbus/memory.go` — in-process impl (Phase 1 default, zero dependencies).
- `adapters/eventbus/redis.go` — Redis pub/sub impl (Phase 2 opt-in).
- Services call `bus.Publish(ctx, event)`. They never know which implementation is active.
- The implementation is selected at startup based on `REDIS_URL` env presence. No code changes between phases.

### 4. Repository is a Port
- `core/ports/repository.go` defines `MonitorRepository`, `HeartbeatRepository`, etc.
- `adapters/repository/mariadb/` — MariaDB impl (default for K8s).
- `adapters/repository/sqlite/` — SQLite impl (dev/edge, CGO-free).
- Both use Bun query builder. Same Go interface, different driver.
- Domain code never imports `database/sql` or Bun directly.

### 5. Wire-Shape Discipline (never serialize a domain type)
- **Never return a `domain.*` struct directly from an HTTP handler.** Domain types are pure
  data and carry **no `json` tags** — marshalling one emits capitalized Go field names AND
  leaks internal fields. This already caused a real bug: the public `GET /api/status/:slug`
  serialized a raw `domain.StatusPage` and **leaked its bcrypt `PasswordHash`** on an
  unauthenticated endpoint (fixed 2026-07: routed through `SPView`/`toSPView`).
- Every endpoint returns an explicit **View struct** defined in the handler (or a response
  DTO defined in the service) with `json:"snake_case"` tags. Secrets (`PasswordHash`,
  `TOTPSecret`, `KeyHash`, API-key plaintext after creation) are **never** in a View — expose
  a boolean like `has_access` / `totp_enabled` instead.
- Frontend and backend field names must match the JSON tag exactly. When adding a field,
  grep the handler's request/response struct for the real `json:"..."` tag — do not guess.
  (e.g. the status-page password field is bound as `access_code`, NOT `password`.)
- The same applies to **request** binding. `POST /api/maintenance` bound the monitor list as
  `monitors` while the frontend sent `monitor_ids`, so the field silently bound to nothing.

### 6. Time Crosses the DB Boundary in UTC — Always
- Rows store a **UTC wall-clock** (`time.Now().UTC()`). A **local-zoned** `time.Time` handed to a
  query is rendered into the SQL as its **local** wall-clock, so the comparison window is silently
  shifted by the server's UTC offset. No error, no warning — just wrong rows.
- This shipped: the heartbeat handler built its range from `time.Now()`, so on a UTC+7 host the
  1h/3h/6h chart ranges queried a window **7 hours in the future** and returned zero rows forever.
  The chart was permanently blank and it looked like a frontend bug. 24h "worked" by accident —
  its window was wide enough to reach back past the skew, which is what made it so confusing.
- Rule: any `time.Time` that reaches a repository is `.UTC()`. Normalize at the **service**
  boundary (see `HeartbeatService.ListByMonitor`) so a careless caller can't reintroduce it.
- **An in-memory fake will NOT catch this** — it compares instants, which are zone-independent,
  so it returns the right rows even when the bound is wrong. To test it, either assert the bound's
  `Location()` is `time.UTC`, or make the fake compare wall-clocks (see `hbWindowRepo`).

### 7. Never Leave a Stub That Returns Success
- A handler that returns `204`/`[]`/`200` without doing the work makes a **dead feature look
  healthy**: every smoke test and status-code assertion passes while nothing happens.
- This shipped too. Maintenance windows suppressed **nothing** for months: `Create` dropped the
  monitor list (`// TODO: assign monitors if provided`), `AssignMonitor`/`UnassignMonitor` returned
  `204` without writing a link, and `ListMonitors` returned a hardcoded `[]`. Every endpoint
  answered 2xx, the UI showed "All monitors", and alerts fired straight through the window.
- If you must land an unfinished endpoint, return **501 Not Implemented**. It is honest, and it
  fails loudly the moment someone depends on it.
- Test the **effect**, not the status code. The load-bearing assertion for suppression is
  `IsActive()` — the thing the scheduler and dispatcher actually consult. A `201` proves nothing.
- Corollary: a fake repo written to accommodate a stub (the old no-op `fakeMaintenanceLinkRepo`)
  cements the bug. If a test double does nothing, ask what it is hiding.

### 8. "The Latest Row" Needs a Deterministic Tie-Break
- `heartbeats.time` is a **second-precision `TIMESTAMP`** on MariaDB (`001_init`). Beats written
  within the same second carry the **same stored value**, so an `ORDER BY time` alone leaves the
  winner to the engine.
- This shipped, and it was ugly: `GetLatest` ordered by `time DESC` only, and MariaDB happily
  returned the **older `PENDING`** row instead of the `DOWN` that had just confirmed it. Everything
  that derives state from "the latest heartbeat" then saw the wrong status — `HeartbeatService.Record`
  takes both `oldStatus` and `prevDownCount` from that call, so a monitor could **re-alert a DOWN it
  had already alerted**, miscount its retry window, or miss a recovery. Folder alerting never fired
  at all. Fixed 2026-07-13: both adapters order by `time DESC, id DESC` (and `time ASC, id ASC` for
  `ListByMonitor`). `id` is monotonic per insert — it is the real chronological order.
- Rule: **any query meaning "the latest row", or any ordered list a human reads as a sequence, needs
  a tie-break on `id`.** Do not assume a timestamp is unique. Assume second precision on MariaDB.
- **SQLite will not produce the tie for you** — it stores higher-precision timestamps, so every
  SQLite-only repo test passed while MariaDB was wrong (see rule 3 territory: the engines differ).
  To test this, **construct the tie**: write two rows with an identical `Time` and assert the order.
  See `TestHeartbeatGetLatest_BreaksTimestampTieByID`.
- Do **not** "fix" this by widening the column to `TIMESTAMP(3)`: `heartbeats` is
  `PARTITION BY RANGE (UNIX_TIMESTAMP(time))`, and a fractional-second column makes that partition
  expression illegal. MariaDB also **truncates** sub-second values rather than rounding, so ties are
  the only symptom — ordering determinism fixes it completely.

## Minimal-Dependency Principle (Non-Negotiable)

The **default deployment** must work with **zero external dependencies**:
- Single pod, single PVC, embedded frontend.
- MariaDB on a PVC (or SQLite for dev) — no external DB required.
- In-process EventBus — no Redis required.
- Frontend embedded in Go binary via `//go:embed web/dist` — no nginx, no separate web Deployment required.

Redis, external MariaDB, and separate web tier are **opt-in via Helm values**. They must NOT be required for the tool to function. If your code requires Redis or an external service to boot, it is a bug.

## Tech Stack (Locked)

| Layer | Choice | Do NOT substitute |
|---|---|---|
| Backend language | Go 1.23+ | ❌ Rust, ❌ Node/TS, ❌ Python |
| HTTP framework | Echo v4 | ❌ Gin, ❌ Fiber, ❌ net/http raw |
| WebSocket | `coder/websocket` | ❌ gorilla/websocket (maintenance mode) |
| Query builder | Bun | ❌ GORM, ❌ Ent, ❌ raw database/sql in services |
| MariaDB driver | `go-sql-driver/mysql` | |
| SQLite driver | `modernc.org/sqlite` (CGO-free) | ❌ mattn/go-sqlite3 (requires CGO) |
| Auth | `golang-jwt/jwt/v5` + `pquerna/otp` (TOTP) | |
| Password | `golang.org/x/crypto/bcrypt` | |
| ICMP | `prometheus-community/pro-bing` | ❌ go-ping/ping (deprecated) |
| DNS | `miekg/dns` | |
| MQTT | `eclipse/paho.mqtt.golang` | |
| RabbitMQ | `github.com/rabbitmq/amqp091-go` | |
| gRPC | `google.golang.org/grpc` + health/v1 | |
| SNMP | `gosnmp/gosnmp` | |
| Docker | `docker/docker/client` | |
| Frontend | Svelte 5 + SvelteKit | ❌ React, ❌ Vue, ❌ Angular |
| Package manager | **Bun** | ❌ npm, ❌ pnpm, ❌ yarn |
| Build | Vite 5 | |
| i18n | `inlang/paraglide-js` v2 | |
| UI primitives | `shadcn-svelte` (wraps `bits-ui`) | |
| Charts | `LayerCake` + D3 scales | ❌ Chart.js, ❌ ApexCharts |
| Icons | `lucide-svelte` | |
| Toasts | `svelte-sonner` | |
| Forms | `sveltekit-superforms` + `zod` | |
| CSS | Tailwind CSS v4 | |
| Database | **MariaDB** (primary), SQLite (dev) | ❌ PostgreSQL (can be added as 3rd adapter later, not now) |
| Migrations | `bun/migrate` | |
| Logging | `log/slog` (stdlib, JSON) | ❌ logrus, ❌ zap |
| Metrics | `prometheus/client_golang` | |
| Config | `caarlos0/env/v11` + env vars | ❌ spf13/viper for app config (OK for Helm values) |
| Scheduling | `robfig/cron/v3` | |

**All Go libraries must be CGO-free** so the binary cross-compiles with `CGO_ENABLED=0`.

## Monitor Types (12 — do NOT add more without user approval)

http, tcp, ping, dns, websocket, push, docker, mqtt, rabbitmq, grpc, snmp, database

**Database monitor engines (user-approved):** postgres, mysql, mariadb, mongodb, redis, mssql.
Health checks use fixed presets only (`ping` / `select_1`) — never free-form operator SQL.
Optional session-pool (`check_session_pool`) and storage (`check_storage`) checks also use
fixed engine queries only. Over-threshold is a typed capacity `warning`; missing privilege is
condition `error`; both keep primary availability UP and are never silently skipped. Warning,
error, and recovery promote after two consecutive samples. Capacity never becomes heartbeat DOWN.

**Explicitly excluded (do NOT add):** systemd (deferred until Uptime Phoenix agent), gamedig, tailscale, kafka, radius.

## Notification Providers (11 — do NOT add more without user approval)

telegram, discord, slack, smtp, webhook, teams, mattermost, gotify, bark, feishu, line

## Auth & User Management (established 2026-07 — follow this model)

- **Open self-registration is DISABLED.** `POST /api/auth/register` only works to bootstrap the
  very first user (returns **403** once any user exists). Do not re-open it. New users after the
  first are created by an admin via `POST /api/users`.
- **RBAC (expanded 2026-07-12, migration `008` — user-approved).** `domain.User.IsAdmin` (migration
  `006`) still exists and still means *unrestricted*: an admin sees and does everything. On top of it:
  - **Scoped visibility.** A non-admin sees ONLY the monitors granted to them — directly, or by
    sitting inside a granted group (recursively, through nested subgroups). No grants ⇒ sees zero
    monitors. Grants live in the `user_permissions` table.
  - **Monitors and groups are READ-ONLY for non-admins**, whatever is granted. Writes carry
    `middleware.RequireAdmin`.
  - **Two independent capability flags** on `domain.User`: `can_manage_notifications` and
    `can_manage_maintenance`, gated by `middleware.RequireCapability`.
  - **`services.AccessService` is the SINGLE authorization choke point.** Do not scatter access
    checks into handlers or repos — extend that service. It is also what scopes the WebSocket hub,
    which previously broadcast every monitor's heartbeats to every client.
  - A monitor the caller may not see returns **404, not 403** — never confirm its existence.
  - `/api/auth/me` reports the **RAW** capability flags, not the effective permission: an admin has
    both flags `false` yet may do everything. **Every UI gate must be `is_admin || can_manage_x`.**

  There is still no general roles table. Do not add further permission dimensions without user approval.
- **User management API — `/api/users` (admin-only):** Create/List/Get/Update/Delete, guarded by
  `middleware.SessionOrAPIKey(authSvc, apiKeyRepo, "write")` **then** `middleware.RequireAdmin(authSvc)`.
  It accepts **either** a session JWT (`Authorization: Bearer <jwt>`) **or** an API key
  (`Authorization: ApiKey <phx_…>` or `X-API-Key: <phx_…>`) that has the `write` scope; in both
  cases the resolved principal must be an admin. This is the supported path for programmatic user creation.
- **Context user-ID key:** middleware stores the authenticated user ID under `ContextUserIDKey`
  (value `"userID"`). Always use the constant, never the literal string, and always read it via the
  `userIDFromContext` helper. (A stray literal `"user_id"` in the API-key middleware was a latent bug.)
- **Delete guards:** never allow deleting your own account, the last remaining user, or the last
  remaining admin — return 409 with a typed sentinel (`ErrDeleteSelf` / `ErrLastUser` / `ErrLastAdmin`).
- **Password / secret rules:** passwords are bcrypt-hashed via the `Authenticator` port (min 8 chars);
  API keys are `phx_`-prefixed, shown once, stored as SHA-256. Never log or return any of these.

## Working in Parallel (Multi-Agent)

When work is fanned out to multiple agents at once:
- **Partition by disjoint file ownership.** Two agents must never edit the same file. A clean split
  is backend (`internal/**`) vs frontend-domain-A vs frontend-domain-B; name the exact files each owns.
- **Write a shared contract first** (API request/response shapes + decisions) and have every agent
  build to it, so a backend and a frontend agent align without seeing each other's diffs. Confirm the
  real `json:"..."` tags from the handler structs before writing the contract — do not guess field names.
- **Read-only audit agents** must not edit source; they write a single report file nobody else touches.
- **The integrator (main) verifies, never trusts reports:** after agents finish, run the full gate
  yourself (below) and reconcile field-name mismatches before declaring done.

## Code Style

### Go
- Run `golangci-lint` before finishing any task. Fix all warnings.
- Use `context.Context` as the first parameter in every function that does I/O.
- No `panic()` in service or adapter code. Return errors.
- No `log.Println` or `fmt.Println` in production code. Use `slog`.
- Structs in `core/domain/` have no methods that do I/O. They are data.
- Interfaces in `core/ports/` are small (1-5 methods). Prefer many small interfaces over one large one.
- Every exported function has a doc comment.
- Error wrapping: `fmt.Errorf("doing X: %w", err)` — preserve the chain.
- No interface pollution: don't define an interface in `ports/` unless there are ≥2 implementations or a test mock needs it.

### Svelte / TypeScript
- **Package manager is Bun. Never run `npm`/`npx`/`pnpm`/`yarn`.** Use `bun install`, `bun add -d <pkg>`,
  `bun run check`, `bun run build`. (`bun run check` = `svelte-kit sync && svelte-check` — the real type gate.)
- Run `eslint` and `prettier` before finishing any task.
- Use Svelte 5 runes (`$state`, `$derived`, `$effect`) — NOT Svelte 4 `$:` reactive syntax.
- Runes in modules must use `.svelte.ts` extension.
- No `any` type without a comment explaining why.
- Every component has a `<script lang="ts">` block.
- Use `$props()` not `export let` for component props.

## Testing

- **Domain services:** unit tests with mocked ports. No DB, no HTTP, no real I/O.
- **Adapters:** integration tests with real DB (testcontainers or local MariaDB/SQLite).
- **Monitor checkers:** each checker has a test that verifies `Check()` returns correct status for a known-good and known-bad target.
- **Notification senders:** each sender has a test that hits a mock HTTP server and verifies the request shape.
- **E2E:** Playwright tests for the critical user journey (login → create monitor → see heartbeat → configure notification → view status page).
- Run `go test ./...` before finishing. All tests must pass.

## File Placement Rules

| You are adding... | It goes in... |
|---|---|
| A new domain type | `internal/core/domain/` |
| A new port interface | `internal/core/ports/` |
| A new use case | `internal/core/services/` |
| A new HTTP handler | `internal/adapters/http/handlers/` |
| A new WebSocket event | `internal/adapters/ws/events.go` + `web/src/lib/stores/ws.svelte.ts` |
| A new monitor type | `internal/adapters/checker/<type>.go` + one line in `registry.go` |
| A new notification provider | `internal/adapters/notifier/<provider>.go` + one line in `registry.go` |
| A new DB migration | `internal/adapters/repository/mariadb/migrations/NNN_name.up.sql` + `.down.sql` |
| A new Svelte page | `web/src/routes/(admin)/<page>/+page.svelte` (admin) or `web/src/routes/(public)/<page>/+page.svelte` (public) |
| A new Svelte component | `web/src/lib/components/` |
| A new runes store | `web/src/lib/stores/<name>.svelte.ts` |
| A new Helm template | `charts/uptime-phoenix/templates/` |
| A new Go dependency | Add to `go.mod`, verify CGO-free, update `docs/ARCHITECTURE.md` appendix |

## Commit Message Format

```
<type>(<scope>): <description>

[optional body]
```

Types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`, `ci`, `perf`
Scopes: `core`, `checker`, `notifier`, `http`, `ws`, `db`, `web`, `helm`, `auth`, `monitor`, `status-page`, `i18n`

Examples:
- `feat(checker): add MQTT broker monitor type`
- `fix(ws): handle reconnection after server restart`
- `docs(ARCHITECTURE): update notification provider list`

## Before You Finish Any Task

> **Full testing guide:** See `docs/TESTING.md` for detailed commands, manual checklists,
> regression areas, and common failure fixes.

1. ✅ `go build ./...` succeeds
2. ✅ `go test -race -count=1 ./...` passes
3. ✅ `golangci-lint run` passes with zero warnings on new code (if not installed, run `go vet ./internal/...` + `gofmt -l internal/` must be empty)
4. ✅ `cd web && bun run check` passes with **zero errors** (if you touched frontend) — this is the type gate
5. ✅ `cd web && bun run build` succeeds (if you touched frontend)
6. ✅ No imports of frameworks/drivers in `internal/core/`
7. ✅ No new external dependencies added without checking CGO-free + updating ARCHITECTURE.md
8. ✅ If you added a monitor type or notification provider: one file + one line in registry.go
9. ✅ If you touched the database: migration files (up + down) created
10. ✅ If you touched K8s manifests: `helm lint charts/uptime-phoenix` passes

Report what you changed, which files, and any decisions you made.
