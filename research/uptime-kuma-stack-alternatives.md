# Uptime-Kuma-Class Monitoring Tool — Stack & Architecture Research

> Companion to `uptime-kuma.md` (2026-06-22)
> Goal: identify the most suitable modern stack to build an Uptime-Kuma-equivalent self-hosted monitoring tool, with **Port and Adapter (Hexagonal) architecture as a strong preference**.

---

## 1. Functional Surface to Preserve

Any replacement for Uptime Kuma must keep the following capabilities. These become the "ports" in the new architecture.

| Capability | Notes |
|---|---|
| Monitor types | HTTP(s), TCP, Ping (ICMP), DNS, WebSocket, MQTT, SNMP, MongoDB, MySQL, Postgres, MSSQL, Redis, gRPC, Kafka, RADIUS, Docker container, systemd service, game server, push (inbound), Tailscale |
| Notification providers | 90+ integrations (Telegram, Discord, Slack, Email/SMTP, Pushover, Gotify, Ntfy, Signal, Teams, Mattermost, PagerDuty, OpsGenie, webhook, etc.) |
| Real-time UI | Single persistent WebSocket pushes heartbeats and CRUD changes |
| Public status pages | Multiple per install, custom domain mapping, incident management |
| Storage | Hot-pluggable: SQLite default, Postgres/MySQL/MSSQL/Oracle in production |
| Auth | Single-user, password + 2FA (TOTP) |
| i18n | Multi-language UI (community translations) |
| Observability | `/metrics` Prometheus endpoint, structured logs |
| Deployment | Single binary Docker image, native Node, Cloudflare Tunnel option |
| 2FA | TOTP |

The Port-and-Adapter map is roughly:

```
PORT                                              ADAPTER
─────────────────────────────────────────         ──────────────────────────────
MonitorRepository (heartbeats, monitors)   ←───  SQLite / Postgres / MySQL / MSSQL
EventBus (realtime pub/sub)                ←───  WebSocket / SSE
MonitorChecker (per protocol family)        ←───  http / tcp / ping / dns / mqtt / ...
NotificationSender                          ←───  telegram / slack / discord / smtp / ...
Clock (schedule evaluation)                 ←───  system clock + croner
Authenticator                               ←───  bcrypt + TOTP + OIDC
MetricsExporter                             ←───  Prometheus client
Logger                                      ←───  log/slog → stdout
PublicStatusPageRenderer                    ←───  LiquidJS / html/template
ConfigProvider                              ←───  env / yaml / flags
```

The domain (use cases) depends only on these ports. Each adapter is a single file (or single package) and is swappable.

---

## 2. The Two Top Candidates

After researching the modern landscape, two stacks are worth serious comparison. (Node/TypeScript, Rust, Elixir, and Python are considered but ranked lower — see §5.)

### 2.1 Option A — **Go + Svelte 5** (RECOMMENDED)

| Layer | Choice | Why |
|---|---|---|
| Language | Go 1.23+ | Monitoring ecosystem is Go. Single binary. Great concurrency. Fast dev velocity. |
| HTTP framework | **Echo v4** or **Gin** | Echo has a more structured context + better middleware; Gin is the most popular. Both work for hexagonal. |
| WebSocket | `coder/websocket` (idiomatic) or `gorilla/websocket` (battle-tested) | Both are production-grade. `coder/websocket` is a modern fork of `nhooyr/websocket` with cleaner context handling. |
| ORM / data access | **`sqlc`** + `pgx` for Postgres, `modernc.org/sqlite` for SQLite | sqlc = write SQL, get type-safe Go. Best alignment with hexagonal because domain stays free of ORM types. Bun is the runner-up if you want more ORM comforts. |
| Migrations | `golang-migrate/migrate` or `pressly/goose` | Pure SQL migrations, language-agnostic |
| Validation | `go-playground/validator/v10` | Standard |
| Auth | `golang-jwt/jwt/v5`, `pquerna/otp` (TOTP), `lestrrat-go/jwx` (OIDC) | Mature libs |
| ICMP | `prometheus-community/pro-bing` | The Go standard; supports SO_MARK |
| DNS | `miekg/dns` | Battle-tested |
| MQTT | `eclipse/paho.mqtt.golang` | Eclipse-maintained |
| WebSocket (frontend ↔ backend) | Native browser API or `svelte-realtime` | No Socket.IO equivalent needed |
| Config | `caarlos0/env/v11` (env) + `spf13/viper` (yaml) | Standard |
| Logging | `log/slog` | Stdlib, structured |
| Metrics | `prometheus/client_golang` | The standard |
| Scheduler | `robfig/cron/v3` | Cron expressions |
| **Frontend** | **Svelte 5** + Vite 5 | Modern runes, no VDOM, 60fps under stress, smaller bundles |
| Frontend state | Svelte 5 runes (`$state`, `$derived`, `$effect`) | No Pinia/Redux needed |
| Frontend ↔ backend | Native WebSocket (or `svelte-realtime`) | One persistent connection |
| Frontend i18n | `inlang/paraglide-js` or `typesafe-i18n` | Both type-safe |
| Charts | `apexcharts` or `LayerCake` (Svelte-native) | Avoid Chart.js Vue wrapper mismatch |
| Icons | `lucide-svelte` (Lucide icons, Svelte-native) | Modern, tree-shakable |
| **Deployment** | Single static binary + single static SPA | CGO-disabled build → no glibc issues |

**Estimated bundle / binary size:** ~25–40 MB Go binary (CGO off) + ~150 KB gzipped SPA.

**Time-to-MVP for Uptime-Kuma parity:** ~3–4 weeks for a single Go-experienced dev (assuming Svelte 5 familiarity).

### 2.2 Option B — **TypeScript/Node + Svelte 5** (closest to Uptime Kuma)

The path of least resistance. Almost a 1:1 port of Uptime Kuma's stack but in TypeScript and with Svelte instead of Vue.

| Layer | Choice | Why |
|---|---|---|
| Language | TypeScript 5.7+ (Node 22+ or Bun 1.x) | Same as Uptime Kuma, but with types |
| HTTP framework | **Fastify** (faster, typed) or **Hono** (ultra-modern, edge-ready) | Fastify is the FastAPI-of-Node; Hono is Bun-native |
| WebSocket | Native `ws` (same as Uptime Kuma) or **Socket.IO 4** | Socket.IO adds reconnection, rooms, fallback — not always needed |
| ORM | **Drizzle** (TypeScript-first, type-safe, SQL-like) or **Prisma** (Tianji uses this) | Drizzle aligns better with hexagonal (you write SQL); Prisma is more popular but has a heavier runtime |
| DB drivers | `better-sqlite3` (sync, fast) / `pg` / `mysql2` | Standard |
| Validation | `zod` 3.x (or 4.x once stable) | Standard |
| Auth | `lucia-auth/lucia` (gone) → `oslo` + `arctic` (OIDC) + `otpauth` (TOTP) | Or `@auth/core` (Auth.js) |
| Real-time | Native `ws` + a thin pub/sub bus (`mitt`) | Simpler than Socket.IO if no fallback needed |
| **Frontend** | Svelte 5 + SvelteKit (or Vite SPA) | Same as Option A |
| Build | Vite 5 / SvelteKit | SvelteKit is the meta-framework; Vite-SPA is fine for a non-public web service |

**Pros:** Lowest risk, biggest ecosystem, easiest hire.
**Cons:** Larger binary/Docker image, higher memory (~60–80 MB baseline), V8 GC pauses, weaker fit for ICMP / raw-socket monitoring (Node's permission model is awkward for ICMP — Uptime Kuma forked a C++ `ping` lib for this reason).

**Reference project:** [msgbyte/tianji](https://github.com/msgbyte/tianji) — the leading Uptime-Kuma-inspired project. It uses TypeScript + Prisma + React. Apache 2.0. 3k+ stars, very active.

### 2.3 Why Go Wins Over Node for This Specific Problem

The single decisive reason is **ICMP and raw-socket probes**. Uptime Kuma had to fork the Node `ping` library to handle ICMP correctly because Node doesn't expose raw sockets to unprivileged code. In Go, `prometheus-community/pro-bing` (and `golang/x/net/icmp`) makes ICMP a 5-line operation:

```go
pinger, err := probing.NewPinger("example.com")
pinger.Count = 3
err = pinger.Run() // blocks until done
stats := pinger.Statistics() // RTT, packet loss, etc.
```

No fork, no native compilation, no permission gymnastics. For a monitoring tool — where ICMP is a first-class check type — this is decisive.

Secondary reasons:
- **Single static binary** → trivially cross-compiles for linux/amd64, linux/arm64, linux/arm/v7, darwin, freebsd.
- **No CGO** → the entire tool can be a CGO-disabled build, meaning the Docker image can be `FROM scratch` for a 20 MB final image.
- **Memory** → Go uses ~30 MB RSS for a loaded agent vs Node's ~80 MB.
- **Concurrency** → goroutines + channels map 1:1 to "many parallel monitor checks". Node is fine for this but Go's scheduler is more predictable for tens of thousands of concurrent timers.

---

## 3. The Go + Svelte 5 Port-and-Adapter Reference Architecture

This is the recommended target architecture. It's small enough to ship, large enough to grow.

### 3.1 Repository layout

```
phoenix/
├── cmd/
│   └── phoenix/
│       └── main.go                   # wire everything
├── internal/
│   ├── core/                         # = HEXAGON CENTER
│   │   ├── domain/                   # pure types (no stdlib imports)
│   │   │   ├── monitor.go
│   │   │   ├── heartbeat.go
│   │   │   ├── status.go
│   │   │   ├── notification.go
│   │   │   └── errors.go
│   │   ├── ports/                    # interfaces only
│   │   │   ├── repository.go         # MonitorRepo, HeartbeatRepo, ...
│   │   │   ├── notifier.go           # NotificationDispatcher
│   │   │   ├── checker.go            # Checker interface (HTTP, TCP, Ping, ...)
│   │   │   ├── eventbus.go           # EventPublisher, EventSubscriber
│   │   │   ├── scheduler.go          # JobScheduler
│   │   │   ├── clock.go              # Clock
│   │   │   └── auth.go               # Authenticator, TwoFactor
│   │   └── services/                 # USE CASES — depend only on ports
│   │       ├── monitor_service.go
│   │       ├── heartbeat_service.go
│   │       ├── notification_service.go
│   │       ├── statuspage_service.go
│   │       └── auth_service.go
│   └── adapters/                     # = HEXAGON EDGES
│       ├── http/                     # primary (inbound) — Gin/Echo handlers
│       │   ├── router.go
│       │   ├── middleware/
│       │   └── handlers/
│       │       ├── monitor.go
│       │       ├── status_page.go
│       │       └── ...
│       ├── ws/                       # primary (inbound) — WebSocket
│       │   ├── hub.go                # connection manager + pub/sub
│       │   └── events.go             # event name constants
│       ├── repository/               # secondary (outbound) — DB
│       │   ├── sqlite/
│       │   │   ├── repo.go
│       │   │   └── migrations/
│       │   └── postgres/
│       │       ├── repo.go
│       │       └── migrations/
│       ├── checker/                  # secondary — monitor types
│       │   ├── http.go               # implements ports.Checker
│       │   ├── tcp.go
│       │   ├── ping.go               # uses prometheus-community/pro-bing
│       │   ├── dns.go                # uses miekg/dns
│       │   ├── mqtt.go               # uses eclipse/paho.mqtt.golang
│       │   ├── snmp.go               # uses gosnmp/gosnmp
│       │   ├── mongodb.go
│       │   ├── postgres.go
│       │   ├── redis.go
│       │   ├── docker.go
│       │   ├── systemd.go
│       │   ├── gamedig.go
│       │   ├── push.go
│       │   ├── tailscale.go
│       │   └── registry.go           # auto-registers all checkers
│       ├── notifier/                 # secondary — notification providers
│       │   ├── telegram.go           # implements ports.Sender
│       │   ├── discord.go
│       │   ├── slack.go
│       │   ├── smtp.go
│       │   ├── webhook.go
│       │   ├── signal.go
│       │   ├── ntfy.go
│       │   ├── pushover.go
│       │   ├── teams.go
│       │   ├── mattermost.go
│       │   ├── pagerduty.go
│       │   ├── opsgenie.go
│       │   ├── nostr.go
│       │   └── registry.go
│       ├── auth/                     # secondary
│       │   ├── jwt.go
│       │   ├── totp.go               # uses pquerna/otp
│       │   ├── oidc.go               # uses lestrrat-go/jwx
│       │   └── password.go           # uses golang.org/x/crypto/bcrypt
│       ├── scheduler/                # secondary
│       │   └── croner.go             # uses robfig/cron/v3
│       ├── metrics/                  # secondary
│       │   └── prometheus.go         # uses prometheus/client_golang
│       └── logger/                   # secondary
│           └── slog.go               # uses log/slog
├── web/                              # Svelte 5 SPA
│   ├── package.json
│   ├── vite.config.ts
│   ├── svelte.config.js
│   └── src/
│       ├── App.svelte
│       ├── main.ts
│       ├── lib/
│       │   ├── ws/                   # WebSocket client
│       │   │   ├── connection.svelte.ts   # runes-based state
│       │   │   └── events.ts
│       │   ├── stores/               # $state-based stores
│       │   │   ├── monitors.svelte.ts
│       │   │   ├── notifications.svelte.ts
│       │   │   └── status.svelte.ts
│       │   ├── i18n/                 # paraglide-js
│       │   ├── api/                  # fetchers (small, only for non-WS endpoints)
│       │   └── ui/                   # design system components
│       ├── routes/                   # if using SvelteKit
│       │   ├── +layout.svelte
│       │   ├── +page.svelte          # dashboard
│       │   ├── status/[slug]/+page.svelte
│       │   └── settings/
│       └── styles/
│           ├── tokens.scss
│           └── app.scss
├── deploy/
│   ├── Dockerfile                    # multi-stage, CGO_ENABLED=0
│   ├── docker-compose.yml
│   ├── healthcheck.go                # small Go binary for Docker HEALTHCHECK
│   └── k8s/
├── go.mod
├── go.sum
├── pnpm-workspace.yaml
└── README.md
```

### 3.2 The one-file plugin convention, type-system enforced

In Uptime Kuma, the plugin convention is by convention. In Go + hexagonal, it can be **enforced by the type system**:

```go
// internal/core/ports/checker.go
package ports

import "context"

type CheckResult struct {
    Status     Status
    LatencyMs  int64
    Message    string
    Metadata   map[string]string
}

type Checker interface {
    Type() string                                            // "http", "tcp", "ping", ...
    Validate(config map[string]any) error                    // config validation
    Check(ctx context.Context, config map[string]any) (CheckResult, error)
}

// internal/adapters/checker/http.go
package checker

import "github.com/fiztoz/uptime-phoenix/internal/core/ports"

type HTTPChecker struct{}

func (HTTPChecker) Type() string { return "http" }
func (HTTPChecker) Validate(c map[string]any) error { /* ... */ }
func (HTTPChecker) Check(ctx context.Context, c map[string]any) (ports.CheckResult, error) { /* ... */ }

// internal/adapters/checker/registry.go
package checker

import "github.com/fiztoz/uptime-phoenix/internal/core/ports"

var registry = map[string]ports.Checker{}

func Register(c ports.Checker) { registry[c.Type()] = c }
func Get(t string) (ports.Checker, bool) { c, ok := registry[t]; return c, ok }

func init() {
    Register(HTTPChecker{})
    Register(TCPChecker{})
    Register(PingChecker{})
    Register(DNSChecker{})
    // ... one line per monitor type
}
```

Every new monitor type is a **single file** that implements the `Checker` interface. The compiler enforces the contract. Tests run in isolation. Adding the 14th monitor type is a 1-file PR.

### 3.3 Real-time via WebSocket + Svelte 5 runes

Server side (Go):

```go
// internal/adapters/ws/hub.go
type Hub struct {
    register   chan *Client
    unregister chan *Client
    publish    chan Event          // core publishes here
    clients    map[*Client]bool
}

func (h *Hub) Run() {
    for {
        select {
        case c := <-h.register:   h.clients[c] = true
        case c := <-h.unregister: delete(h.clients, c)
        case ev := <-h.publish:   h.broadcast(ev)
        }
    }
}

// in services/heartbeat_service.go:
func (s *HeartbeatService) Record(ctx context.Context, hb Heartbeat) error {
    if err := s.repo.Save(ctx, hb); err != nil { return err }
    s.bus.Publish(ctx, Event{Type: "heartbeat", Payload: hb})
    return nil
}
```

The service **doesn't know WebSocket exists** — it just publishes to the `EventBus` port. The WebSocket adapter subscribes.

Client side (Svelte 5):

```ts
// web/src/lib/ws/connection.svelte.ts
import { browser } from '$app/environment';

class RealtimeStore {
  monitors = $state<Monitor[]>([]);
  heartbeats = $state<Map<string, Heartbeat>>(new Map());
  connected = $state(false);

  private ws?: WebSocket;

  connect() {
    if (!browser) return;
    this.ws = new WebSocket('/api/ws');
    this.ws.addEventListener('open', () => (this.connected = true));
    this.ws.addEventListener('close', () => {
      this.connected = false;
      setTimeout(() => this.connect(), 1000); // simple reconnect
    });
    this.ws.addEventListener('message', (e) => this.handle(JSON.parse(e.data)));
  }

  private handle(msg: WsMessage) {
    if (msg.type === 'heartbeat') {
      this.heartbeats.set(msg.payload.monitorId, msg.payload);
    }
    if (msg.type === 'monitor.list') {
      this.monitors = msg.payload;
    }
  }
}

export const realtime = new RealtimeStore();
```

```svelte
<!-- web/src/routes/+page.svelte -->
<script lang="ts">
  import { realtime } from '$lib/ws/connection.svelte';

  $effect(() => { realtime.connect(); });
</script>

{#if !realtime.connected}
  <div class="disconnected-banner">Reconnecting…</div>
{/if}

{#each realtime.monitors as m (m.id)}
  <MonitorCard {m} heartbeat={realtime.heartbeats.get(m.id)} />
{/each}
```

No virtual DOM diffing for 1000 monitor cards updating every 20s. Svelte 5's fine-grained reactivity means only the affected DOM nodes update. Benchmarks show 60fps sustained under stress where React 19 dropped to 30fps in the same scenario.

### 3.4 Concrete tech-stack picks (2025–2026)

| Concern | Pick | Why this one |
|---|---|---|
| HTTP framework | **`labstack/echo/v4`** | Strong typed context, built-in middleware (CORS, rate-limit, JWT, recover), excellent error handling, HTTP/2 + WebSocket support out of the box. Gin is the runner-up if you value ecosystem size. |
| WebSocket | **`coder/websocket`** (formerly nhooyr) | Idiomatic Go, context-aware, graceful shutdown built in, no external dependencies. Gorilla is the alternative if you want maximum community size. |
| ORM / data access | **`sqlc`** for queries, **`pgx/v5`** for the Postgres driver, **`modernc.org/sqlite`** for SQLite (CGO-free) | sqlc keeps the domain SQL-pure and the generated code is type-safe Go. `pgx` is the fastest Postgres driver in Go. SQLite via `modernc.org/sqlite` avoids CGO and enables static binaries. |
| Migrations | **`golang-migrate/migrate`** (CLI) with versioned `.sql` files | Simple, language-agnostic, plays well with sqlc |
| Config | **`caarlos0/env/v11`** (env) + **`spf13/viper`** for yaml fallback | Standard Go combo |
| Validation | **`go-playground/validator/v10`** | Echo integrates it natively |
| JWT | **`golang-jwt/jwt/v5`** | The standard |
| OIDC | **`lestrrat-go/jwx/v2`** | Full feature set, well-maintained |
| TOTP/2FA | **`pquerna/otp`** | The standard |
| Password hashing | **`golang.org/x/crypto/bcrypt`** | Stdlib-quality |
| ICMP | **`prometheus-community/pro-bing`** | Used by Prometheus ping_exporter; supports SO_MARK |
| DNS | **`miekg/dns`** | The de facto Go DNS lib |
| MQTT | **`eclipse/paho.mqtt.golang`** | Eclipse-maintained |
| SNMP | **`gosnmp/gosnmp`** | De facto Go SNMP |
| MongoDB | **`mongodb/mongo-go-driver`** | Official |
| Redis | **`redis/go-redis/v9`** | Most popular |
| Postgres | **`jackc/pgx/v5`** | Fastest PG driver in Go |
| MySQL | **`go-sql-driver/mysql`** | The standard |
| MSSQL | **`microsoft/go-mssqldb`** | Official |
| WebSocket client (Svelte) | native browser `WebSocket` | For SPA, nothing else is needed |
| Frontend framework | **Svelte 5.5+ with runes** | Smallest bundles, no VDOM, 60fps real-time |
| Build tool | **Vite 5** | Standard |
| Svelte meta-framework | **SvelteKit** for public status pages, plain Vite-SPA for the admin dashboard | Best of both worlds |
| i18n | **`inlang/paraglide-js`** (type-safe, tree-shakable, no runtime) | Modern standard; replaces `svelte-i18n` and `vue-i18n` |
| Charts | **`LayerCake`** (Svelte-native, composable) or **`apexcharts`** with `svelte-apexcharts` | LayerCake is the Svelte-idiomatic answer to D3 |
| Icons | **`lucide-svelte`** | Tree-shakable Lucide icons |
| Toast | **`svelte-sonner`** | Modern, no-Flash-of-Toast |
| UI primitives | **`bits-ui`** or **`shadcn-svelte`** | Bits UI for headless, shadcn-svelte for styled-but-customizable |
| Forms | **`sveltekit-superforms`** (if SvelteKit) | Best-in-class |
| Tests (backend) | Stdlib `testing` + **`testcontainers-go`** for real DB tests | Same pattern as Uptime Kuma |
| Tests (frontend) | **`vitest`** + **`@playwright/test`** for E2E | Standard |
| Linting | **`golangci-lint`** + **`eslint`** + **`prettier`** | Standard |
| Docker | Multi-stage, `CGO_ENABLED=0`, `FROM gcr.io/distroless/static-debian12` for ~20 MB final | Standard for Go |

### 3.5 Svelte 5 vs the rest (real-time dashboard context)

| Framework | Real-time dashboard fitness | Notes |
|---|---|---|
| **Svelte 5** | ⭐⭐⭐⭐⭐ | 60fps sustained under 100 concurrent rapid interactions; smallest bundles |
| Solid.js | ⭐⭐⭐⭐⭐ | Similar fine-grained reactivity model; smaller community than Svelte |
| Vue 3 (Uptime Kuma) | ⭐⭐⭐ | Sometimes drops to 58fps under stress; larger bundles |
| React 19 (Tianji uses this) | ⭐⭐⭐⭐ | With the React 19 compiler, closes the gap; still a VDOM diff |
| Svelte 4 | ⭐⭐⭐⭐ | Was already good; Svelte 5's runes are cleaner |

Svelte 5's runes (`$state`, `$derived`, `$effect`) are particularly good for the "one persistent WebSocket, lots of small reactive updates" pattern because there's no virtual DOM to diff — each `$state` mutation updates exactly the DOM nodes that read it.

---

## 4. Hexagonal Discipline — How Strictly to Apply

The Port-and-Adapter pattern has a strictness dial. For a self-hosted monitoring tool with 1–3 developers, here's the right setting.

### 4.1 What must be enforced
- **No DB driver imports in `core/services/`.** Period. Even `database/sql` is banned in services.
- **No HTTP framework imports in `core/`.** Echo / Gin must live in `adapters/http/`.
- **Ports are interfaces, defined in `core/ports/`, implemented in `adapters/`.**
- **All cross-boundary data passes through the domain types in `core/domain/`.** Adapters translate to/from domain types at the boundary.

### 4.2 What can be relaxed
- **HTTP and WebSocket can share a hub** in `adapters/ws/hub.go` since they both serve the same event bus.
- **Migrations can live next to the DB adapter** (`adapters/repository/sqlite/migrations/`), since they are SQLite-specific.
- **Composition root (`cmd/phoenix/main.go`) is allowed to import anything** — that's its job.
- **Logging can pass through `slog`** without a port, IF you accept the stdlib dependency. (If you want to be strict, define a `Logger` port in `core/ports/` and have `slog` be the default adapter.)

### 4.3 What to avoid
- **Don't add a `repository.New(db)` constructor in `core/services/`.** The service must receive an already-constructed `MonitorRepository` (interface) in its constructor.
- **Don't use `context.Context` in domain types.** `ctx` belongs at the use-case boundary, not in the model.
- **Don't define ports for things you won't swap.** If you'll never use MySQL, don't define a `DB` port — define a `MonitorRepository` port and a `PostgresMonitorRepository` adapter. The port is named after the *role*, not the *technology*.

---

## 5. The Candidates I Considered and Ranked

| Rank | Stack | Pros | Cons | Verdict |
|---|---|---|---|---|
| 1 | **Go + Svelte 5** | Single binary, monitoring ecosystem, ICMP-native, fast dev, type-safe hexagonal | Go dev velocity is 2× faster than Rust but slower than TS for UI-heavy work | **Pick this** |
| 2 | TypeScript/Node + Svelte 5 | Easiest to start, biggest ecosystem, Tianji is a working reference | ICMP awkward (forked lib), heavier binary, Node GC pauses, larger Docker image | Good fallback if team is TS-first |
| 3 | Rust + Svelte 5 | Type safety end to end, smallest memory, no GC | 2× dev time vs Go, ICMP/socket ecosystem smaller, fewer HTTP framework choices | Worth it only at hyperscale or if team is Rust-first |
| 4 | Elixir + Phoenix LiveView | Built for real-time (Phoenix Channels), great concurrency, OTP supervision | Smaller ecosystem for monitoring protocols (ICMP works via Erlang `:erlang`), smaller dev pool | Strong choice if real-time is the primary product; weaker for raw-socket monitoring |
| 5 | Python (FastAPI) + Svelte 5 | Massive ecosystem, easy dev, asyncio for concurrency | GIL considerations, larger memory, ICMP only via `scapy` (requires root) | Skip — Python is great for data tools, weak for long-running system services |
| 6 | C#/.NET + Svelte 5 | Mature, single-file deployment, AOT compilation | Smaller monitoring ecosystem, fewer devs in our context, heavier framework | Not the right fit |

---

## 6. Reference Projects Worth Studying

- **[Tianji](https://github.com/msgbyte/tianji)** (3k+ stars) — Apache 2.0, TypeScript + Prisma + React, monorepo with pnpm, a direct Uptime-Kuma successor with analytics + uptime + server status. Best TS reference.
- **[Uptime Kuma](https://github.com/louislam/uptime-kuma)** (60k+ stars) — MIT, Vue 3 + Express + redbean, the original. Best architecture reference (real-time-first SPA + one-file plugins).
- **[gohexaclean](https://github.com/gieart87/gohexaclean)** — Fiber + GORM boilerplate with strict Hexagonal + Clean layering.
- **[prometheus-community/pro-bing](https://github.com/prometheus-community/pro-bing)** — Reference ICMP implementation.
- **[ping_exporter](https://github.com/czerwonk/ping_exporter)** — Prometheus exporter for ICMP built on pro-bing.

---

## 7. Decisions Locked (after user feedback, 2026-06-22)

1. **Public status pages** — same SvelteKit app as the admin, but built as **SSG** and served as a separate `phoenix-web` K8s Deployment. Two routes in one SvelteKit project, deployed as static assets.
2. **Tauri v2 desktop wrapper** — **No, web only** for the early phase. A Tauri shell can be a thin wrapper around the same web build later.
3. **2FA scope** — **TOTP only** for the early phase (via `pquerna/otp`). The `core/ports/auth.go` interface is designed so WebAuthn can be added as a second adapter without touching the auth service.

Additional locked decisions that affect the architecture:

4. **K8s-native from day 1.** The repo ships a `charts/uptime-phoenix/` Helm chart that brings up the full stack with `helm install`. Architecture is K8s-shaped from the start.
5. **Frontend and backend in separate K8s Deployments.** `phoenix-web` (Svelte SPA + nginx) and `phoenix-api` (Go binary) scale independently. Different images, different release cadence, different security boundaries.

See `research/uptime-kuma-k8s-architecture.md` for the full K8s architecture and the 3-phase rollout (monolith → split worker/API → sharded workers).

---

## 8. TL;DR Recommendation

> **Go 1.23+ + Svelte 5, in a strict Port-and-Adapter (hexagonal) layout.**
>
> - Backend: Go with `labstack/echo/v4` (HTTP) + `coder/websocket` (WS) + `sqlc` + `pgx` (Postgres) + `modernc.org/sqlite` (SQLite, CGO-free).
> - Adapters live in `internal/adapters/{checker,notifier,repository,http,ws,auth,metrics}/`. Each monitor type and each notification provider is a single file implementing one interface.
> - Frontend: Svelte 5 + SvelteKit (public) + Vite-SPA (admin) + Vite 5 build. Runes-based reactive store consuming a single native WebSocket. `inlang/paraglide-js` for i18n, `LayerCake` or `apexcharts` for charts.
> - Ship a 20–30 MB static binary (CGO_ENABLED=0) + a 200 KB gzipped SPA in a single Docker image (`FROM scratch`).
> - Time-to-Uptime-Kuma-parity for a single Go-fluent dev: **3–4 weeks**.

This gives you:
- **Uptime-Kuma's product surface** (all 13+ monitor types, all notification providers, real-time UI, public status pages)
- **Hot-pluggable database** (SQLite ↔ Postgres, just swap the repository adapter)
- **Type-safe plugin model** (the compiler enforces the `Checker` and `Sender` interfaces)
- **First-class dev velocity** (Go dev is 2× faster than Rust for this class of work, with 95% of the runtime efficiency)
- **The smallest possible production binary** (Go static binary + tiny Svelte SPA)
- **A real-time dashboard that doesn't drop frames** (Svelte 5's fine-grained reactivity)

---

## 9. Sources

- Go vs Rust for monitoring: <https://mpurayil.com/blog/monitoring-observability-go-vs-rust-2025>
- Go hexagonal architecture guide: <https://www.golinuxcloud.com/hexagonal-architecture-golang/>
- Go web framework comparison: <https://buanacoding.com/2025/09/fiber-vs-gin-vs-echo-golang-framework-comparison-2025.html>
- Go ORM comparison: <https://www.glukhov.org/app-architecture/data-access/comparing-go-orms-gorm-ent-bun-sqlc/>
- Tianji (TS reference): <https://github.com/msgbyte/tianji>
- pro-bing (ICMP): <https://github.com/prometheus-community/pro-bing>
- Svelte 5 runes: <https://svelte.dev/blog/runes>
- svelte-realtime: <https://svelte-realtime.dev/>
- Go WebSocket guide: <https://websocket.org/guides/languages/go/>
- Original Uptime Kuma research: `research/uptime-kuma.md` (this directory)
