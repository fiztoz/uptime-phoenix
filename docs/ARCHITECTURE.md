# Uptime Phoenix — Detailed Design Document

> A self-hosted, K8s-native, minimal-dependency monitoring tool.
> Architecture: Port-and-Adapter (Hexagonal). Stack: Go + Svelte 5. Database: MariaDB (SQLite for dev).

---

## Table of Contents

1. [Design Principles](#1-design-principles)
2. [System Architecture](#2-system-architecture)
3. [Port-and-Adapter Layout](#3-port-and-adapter-layout)
4. [Domain Model](#4-domain-model)
5. [Port Interfaces](#5-port-interfaces)
6. [Database Schema (MariaDB)](#6-database-schema-mariadb)
7. [Monitor Types (Checker Adapters)](#7-monitor-types-checker-adapters)
8. [Notification Providers (Sender Adapters)](#8-notification-providers-sender-adapters)
9. [Real-time Layer (WebSocket + EventBus)](#9-real-time-layer-websocket--eventbus)
10. [Frontend Architecture (Svelte 5)](#10-frontend-architecture-svelte-5)
11. [Authentication & 2FA](#11-authentication--2fa)
12. [Status Pages](#12-status-pages)
13. [Scheduler & Heartbeat Lifecycle](#13-scheduler--heartbeat-lifecycle)
14. [Observability](#14-observability)
15. [Deployment (K8s + Helm)](#15-deployment-k8s--helm)

---

## 1. Design Principles

1. **Minimal dependencies by default.** One pod, one PVC, zero external services. `helm install` → works. Redis, external MariaDB, and separate web tier are opt-in, never required.
2. **Domain logic is pure.** `internal/core/` imports no framework, no DB driver, no HTTP library. Only stdlib + domain types.
3. **Every integration is one file.** Each monitor type and each notification provider is a single Go file implementing one interface. The compiler enforces the contract.
4. **EventBus is a port.** In-process in Phase 1, Redis pub/sub in Phase 2. Domain code calls `bus.Publish(ctx, evt)` and never knows the difference.
5. **Database is hot-pluggable.** MariaDB (default) and SQLite (dev) implement the same `Repository` interface. Postgres can be added as a third adapter without touching domain code.
6. **Frontend is embedded by default.** `//go:embed web/dist` serves the SPA from the Go binary. Splitting to a separate nginx Deployment is a Helm value, not a code change.
7. **K8s-native from day 1.** Health probes, graceful shutdown, PDB, ConfigMap/Secret-based config, Helm chart packaging.
8. **Lean.** No required Kafka, RabbitMQ, message brokers, or external DB operators. Only add infrastructure when scale demands it.

---

## 2. System Architecture

### Default Deployment (Phase 1 — single pod, zero external dependencies)

```
                    ┌─────────────────────────────────────────┐
                    │           Browser / Mobile              │
                    │      (Svelte 5 SPA + SvelteKit SSG)    │
                    └──────────────────┬──────────────────────┘
                                       │ HTTPS + WSS
                    ┌──────────────────▼──────────────────────┐
                    │       Ingress (nginx / traefik)         │
                    │       TLS, proxy-read-timeout: 3600     │
                    └──────────────────┬──────────────────────┘
                                       │
                    ┌──────────────────▼──────────────────────┐
                    │         phoenix (single pod)            │
                    │                                         │
                    │  ┌──────────────────────────────────┐   │
                    │  │  Echo HTTP server (:3000)        │   │
                    │  │  ├─ /api/*  → REST handlers      │   │
                    │  │  ├─ /ws    → WebSocket hub       │   │
                    │  │  ├─ /metrics → Prometheus        │   │
                    │  │  └─ /*     → embedded SPA (SSG)  │   │
                    │  └────────────┬─────────────────────┘   │
                    │               │                         │
                    │  ┌────────────▼─────────────────────┐   │
                    │  │  Core Services (use cases)       │   │
                    │  │  MonitorService, HeartbeatService│   │
                    │  │  NotificationService, AuthService│   │
                    │  │  StatusPageService               │   │
                    │  └────┬────────┬──────────┬─────────┘   │
                    │       │        │          │             │
                    │  ┌────▼──┐ ┌──▼────┐ ┌───▼──────────┐  │
                    │  │EventBus│ │Sched-│ │ Checker Pool │  │
                    │  │(in-proc)│ │  uler│ │ (13 types)  │  │
                    │  └────┬──┘ └──┬────┘ └──────────────┘  │
                    │       │       │                        │
                    │  ┌────▼───────▼────────────────────┐   │
                    │  │  Repository (MariaDB or SQLite) │   │
                    │  └────────────────┬────────────────┘   │
                    └───────────────────┼────────────────────┘
                                        │
                         ┌──────────────▼──────────────┐
                         │   PVC (MariaDB data, 10Gi)  │
                         └─────────────────────────────┘
```

### Opt-in Scaled Deployment (Phase 2 — multi-pod)

```
           ┌──────────────┐  ┌──────────────┐  ┌──────────────┐
           │ uptime-phoenix-api │  │ uptime-phoenix-api │  │ uptime-phoenix-api │  (HPA, 2-N)
           │ (stateless)  │  │ (stateless)  │  │ (stateless)  │
           │ HTTP + WS    │  │ HTTP + WS    │  │ HTTP + WS    │
           └──────┬───────┘  └──────┬───────┘  └──────┬───────┘
                  │                 │                 │
                  └────────┬────────┘                 │
                           │  pub/sub                 │
                  ┌────────▼────────┐         ┌───────▼────────┐
                  │     Redis       │         │   MariaDB      │
                  │ (event fan-out) │         │ (source of     │
                  └────────┬────────┘         │   truth)       │
                           │                  └────────────────┘
                  ┌────────▼────────┐
                  │ uptime-phoenix-worker │  (replicas=1, PDB)
                  │ scheduler +     │
                  │ checkers +      │
                  │ notifications   │
                  └─────────────────┘
```

All transitions from default → scaled are **Helm value changes**, not code changes.

---

## 3. Port-and-Adapter Layout

```
phoenix/
├── cmd/
│   ├── app/main.go                    # Phase 1: all-in-one entry point
│   ├── api/main.go                    # Phase 2: stateless API entry point
│   └── worker/main.go                 # Phase 2: scheduler entry point
│
├── internal/
│   ├── core/                          # ══ HEXAGON CENTER ══
│   │   ├── domain/                    # Pure types, no imports
│   │   │   ├── monitor.go
│   │   │   ├── heartbeat.go
│   │   │   ├── status.go
│   │   │   ├── notification.go
│   │   │   ├── status_page.go
│   │   │   ├── tag.go
│   │   │   ├── maintenance.go
│   │   │   ├── user.go
│   │   │   ├── api_key.go
│   │   │   ├── incident.go
│   │   │   └── errors.go
│   │   ├── ports/                     # Interfaces only
│   │   │   ├── repository.go          # MonitorRepo, HeartbeatRepo, etc.
│   │   │   ├── checker.go             # Checker interface
│   │   │   ├── notifier.go            # NotificationSender interface
│   │   │   ├── eventbus.go            # EventBus interface
│   │   │   ├── scheduler.go           # Scheduler interface
│   │   │   ├── auth.go                # Authenticator, TwoFactor
│   │   │   ├── clock.go               # Clock
│   │   │   ├── logger.go              # Logger
│   │   │   ├── metrics.go             # MetricsExporter
│   │   │   └── config.go              # ConfigProvider
│   │   └── services/                  # Use cases — depend only on ports
│   │       ├── monitor_service.go
│   │       ├── heartbeat_service.go
│   │       ├── notification_service.go
│   │       ├── statuspage_service.go
│   │       ├── auth_service.go
│   │       ├── tag_service.go
│   │       ├── maintenance_service.go
│   │       └── aggregate_service.go   # rollup computation
│   │
│   └── adapters/                      # ══ HEXAGON EDGES ══
│       ├── http/                      # Primary (inbound)
│       │   ├── router.go              # Echo router setup
│       │   ├── middleware/            # auth, cors, ratelimit, requestid
│       │   └── handlers/             # one file per resource
│       ├── ws/                        # Primary (inbound) — WebSocket
│       │   ├── hub.go                 # connection manager
│       │   └── events.go              # event name constants
│       ├── repository/                # Secondary (outbound) — DB
│       │   ├── mariadb/               # MariaDB adapter (Bun)
│       │   │   ├── repo.go
│       │   │   └── migrations/
│       │   └── sqlite/                # SQLite adapter (Bun, CGO-free)
│       │       ├── repo.go
│       │       └── migrations/
│       ├── eventbus/                  # Secondary — event transport
│       │   ├── memory.go              # in-process (Phase 1 default)
│       │   └── redis.go               # Redis pub/sub (Phase 2 opt-in)
│       ├── checker/                   # Secondary — monitor types
│       │   ├── http.go                # HTTP(s) + keyword + JSON query + TLS
│       │   ├── tcp.go
│       │   ├── ping.go                # pro-bing (ICMP)
│       │   ├── dns.go                 # miekg/dns
│       │   ├── websocket.go           # coder/websocket
│       │   ├── push.go                # inbound HTTP receiver
│       │   ├── docker.go              # docker/docker/client
│       │   ├── mqtt.go                # eclipse/paho.mqtt.golang
│       │   ├── rabbitmq.go            # github.com/rabbitmq/amqp091-go
│       │   ├── grpc.go                # google.golang.org/grpc health
│       │   ├── snmp.go                # gosnmp/gosnmp
│       │   ├── database.go            # postgres/mysql/mariadb/mssql/mongo/redis; ping + select_1; optional session/storage thresholds
│       │   ├── database_capacity.go   # session-pool / storage queries (fixed SQL) + threshold math
│       │   ├── s3.go                  # AWS / MinIO / S3-compatible signed HeadBucket / HeadObject / GetObject
│       │   ├── s3_sigv4.go            # in-tree SigV4 (no AWS SDK)
│       │   └── registry.go            # auto-registers all checkers
│       ├── notifier/                  # Secondary — 11 notification providers
│       │   ├── telegram.go
│       │   ├── discord.go
│       │   ├── slack.go
│       │   ├── smtp.go                # email
│       │   ├── webhook.go
│       │   ├── teams.go
│       │   ├── mattermost.go
│       │   ├── gotify.go
│       │   ├── bark.go
│       │   ├── feishu.go
│       │   ├── line.go
│       │   ├── ratelimit.go           # shared retry/backoff middleware
│       │   ├── alert_format.go        # shared payload / severity helpers
│       │   └── registry.go
│       ├── auth/                      # Secondary
│       │   ├── jwt.go                 # golang-jwt/jwt/v5
│       │   ├── totp.go                # pquerna/otp
│       │   ├── password.go            # golang.org/x/crypto/bcrypt
│       │   ├── webauthn.go            # go-webauthn
│       │   └── oidc.go                # coreos/go-oidc + golang.org/x/oauth2
│       ├── scheduler/                 # Secondary
│       │   ├── local.go               # in-process (robfig/cron/v3)
│       │   └── sharded.go             # DB-leased multi-worker (shipped)
│       ├── metrics/                   # Secondary
│       │   └── prometheus.go          # prometheus/client_golang
│       └── logger/                    # Secondary
│           └── slog.go                # log/slog
│
├── web/                               # Svelte 5 + SvelteKit frontend
│   ├── src/
│   │   ├── routes/
│   │   │   ├── (admin)/               # SPA mode (ssr=false)
│   │   │   │   ├── dashboard/
│   │   │   │   ├── monitors/
│   │   │   │   ├── settings/
│   │   │   │   └── incidents/
│   │   │   └── (public)/              # SSG mode (prerender=true)
│   │   │       └── [domain]/
│   │   ├── lib/
│   │   │   ├── stores/                # runes-based ($state, $derived)
│   │   │   │   ├── ws.svelte.ts       # WebSocket client + reconnection
│   │   │   │   ├── monitors.svelte.ts
│   │   │   │   └── auth.svelte.ts
│   │   │   ├── components/
│   │   │   │   ├── ui/                # shadcn-svelte primitives
│   │   │   │   └── charts/            # LayerCake charts
│   │   │   └── i18n/                  # paraglide-js output
│   │   └── hooks.server.ts            # auth guard + custom domain routing
│   ├── static/
│   ├── project.inlang/                # i18n config
│   ├── svelte.config.js
│   └── vite.config.ts
│
├── charts/uptime-phoenix/                    # Helm chart
│   ├── Chart.yaml
│   ├── values.yaml
│   └── templates/
│       ├── deployment.yaml            # single-pod (default) or split
│       ├── service.yaml
│       ├── ingress.yaml
│       ├── pvc.yaml
│       ├── secret.yaml
│       ├── configmap.yaml
│       ├── pdb.yaml
│       └── _helpers.tpl
│
├── Dockerfile                         # Go binary, CGO_ENABLED=0, distroless
├── go.mod
├── go.sum
└── Makefile
```

### Dependency Direction Rule

```
cmd/ ──▶ adapters/ ──▶ core/services/ ──▶ core/ports/ ──▶ core/domain/
           │                                              (pure types)
           └── implements core/ports/* interfaces
```

**Adapters depend on core. Core never depends on adapters.**

---

## 4. Domain Model

```go
// internal/core/domain/status.go
package domain

type Status int

const (
    StatusDown       Status = 0
    StatusUp         Status = 1
    StatusPending    Status = 2
    StatusMaintenance Status = 3
)

func (s Status) String() string {
    switch s {
    case StatusUp:         return "UP"
    case StatusDown:       return "DOWN"
    case StatusPending:    return "PENDING"
    case StatusMaintenance: return "MAINTENANCE"
    default:               return "UNKNOWN"
    }
}
```

```go
// internal/core/domain/monitor.go
package domain

type Monitor struct {
    ID              int64
    UserID          int64
    Name            string
    Type            string            // "http", "tcp", "ping", "dns", ...
    Active          bool
    Interval        int               // seconds between checks
    RetryInterval   int               // seconds between retries
    MaxRetries      int
    Timeout         float64           // seconds
    Config          map[string]any    // per-type config (JSONB in DB)
    AcceptedStatusCodes []string      // for HTTP: ["200-299", "301"]
    ProxyID         *int64
    UpsideDown      bool              // flip UP/DOWN
    ResendInterval  int               // minutes between repeated alerts
    PushToken       string            // for push-type monitors
    ParentID        *int64            // for grouping
    Weight          int               // display order
    CreatedAt       time.Time
    UpdatedAt       time.Time
}
```

```go
// internal/core/domain/heartbeat.go
package domain

type Heartbeat struct {
    ID          int64
    MonitorID   int64
    Status      Status
    Time        time.Time
    Msg         string
    Ping        int               // latency in ms (NULL if not measured)
    Duration    int               // total check duration in ms
    Important   bool              // flagged for extended retention
    DownCount   int               // consecutive down count
}
```

```go
// internal/core/domain/monitor_condition.go
package domain

type ConditionState string // ok, warning, error; stale is derived from freshness

type ConditionObservation struct {
    Kind       string       // session_pool or storage
    State      ConditionState
    Used       *float64     // nil means no measurement; zero remains meaningful
    Limit      *float64
    Percent    *float64
    Threshold  *float64
    Unit       string       // connections or bytes
    Resource   string       // engine-specific semantic label
    Scope      string
    Source     string
    Message    string
    ObservedAt time.Time
    StaleAfter time.Time
}

type MonitorCondition struct {
    MonitorID int64
    ConditionObservation
    LastSuccessAt     *time.Time
    ConsecutiveState  ConditionState
    ConsecutiveCount  int
    LastNotifiedState ConditionState
    LastNotifiedAt    *time.Time
}
```

```go
// internal/core/domain/notification.go
package domain

type Notification struct {
    ID         int64
    UserID     int64
    Name       string
    Type       string            // "telegram", "discord", "slack", ...
    Active     bool
    IsDefault  bool
    TemplateID *int64           // nil = provider's built-in layout
    Config     map[string]any    // per-provider config (JSONB in DB)
    CreatedAt  time.Time
    UpdatedAt  time.Time
}

type NotificationTemplate struct {
    ID            int64
    UserID        int64
    Name          string
    Provider      string         // discord, smtp, webhook, or line
    TitleTemplate string         // Discord embed title / SMTP subject
    BodyTemplate  string
    Config        map[string]any // provider-specific layout (Discord embeds / SMTP HTML)
    CreatedAt     time.Time
    UpdatedAt     time.Time
}

type AlertContext struct {
    AlertScope     string         // monitor or group
    MonitorID      int64
    MonitorName    string
    MonitorType    string
    MonitorTarget  string
    GroupID        int64
    GroupName      string
    GroupCondition GroupCondition
    Status         Status
    PreviousStatus Status
    Message        string
    Duration       time.Duration
    StartedAt      time.Time
    CheckOutput    string
    Tags           map[string]string
    EventKind      string         // status_change, certificate_expiry, capacity_condition
    ConditionKind  string
    ConditionState ConditionState
    // ConditionUsed/Limit/Percent/Threshold + unit/resource/scope/source/observed_at
    // are populated only for capacity_condition.
}
```

```go
// internal/core/domain/status_page.go
package domain

type StatusPage struct {
    ID              int64
    Slug            string
    Title           string
    Description     string
    Icon            string
    Theme           string            // "light", "dark", "auto"
    Published       bool
    CustomDomain    string
    PasswordHash    string            // optional protection
    FooterText      string
    CustomCSS       string
    ShowTags        bool
    AutoResolveIncidents bool
    CreatedAt       time.Time
    UpdatedAt       time.Time
}
```

*(Additional domain types: `User`, `Tag`, `MaintenanceWindow`, `APIKey`, `Incident`, `Proxy`, `DockerHost`, `TLSInfo`, `Setting` — same pattern, pure structs.)*

---

## 5. Port Interfaces

```go
// internal/core/ports/repository.go
package ports

import "context"
import "github.com/fiztoz/uptime-phoenix/internal/core/domain"

type MonitorRepository interface {
    Create(ctx context.Context, m *domain.Monitor) error
    GetByID(ctx context.Context, id int64) (*domain.Monitor, error)
    List(ctx context.Context, filter MonitorFilter) ([]*domain.Monitor, error)
    ListActive(ctx context.Context) ([]*domain.Monitor, error)
    Update(ctx context.Context, m *domain.Monitor) error
    Delete(ctx context.Context, id int64) error
}

type HeartbeatRepository interface {
    Save(ctx context.Context, h *domain.Heartbeat) error
    GetLatest(ctx context.Context, monitorID int64) (*domain.Heartbeat, error)
    ListByMonitor(ctx context.Context, monitorID int64, from, to time.Time) ([]*domain.Heartbeat, error)
    SaveAggregate1m(ctx context.Context, agg *Aggregate1m) error
    SaveAggregate1h(ctx context.Context, agg *Aggregate1h) error
    SaveAggregate1d(ctx context.Context, agg *Aggregate1d) error
    GetAggregate1m(ctx context.Context, monitorID int64, from time.Time) ([]*Aggregate1m, error)
    GetAggregate1h(ctx context.Context, monitorID int64, from time.Time) ([]*Aggregate1h, error)
    GetAggregate1d(ctx context.Context, monitorID int64, from time.Time) ([]*Aggregate1d, error)
}

type MonitorConditionRepository interface {
    Upsert(ctx context.Context, condition *domain.MonitorCondition) error
    Get(ctx context.Context, monitorID int64, kind string) (*domain.MonitorCondition, error)
    ListAll(ctx context.Context) ([]*domain.MonitorCondition, error)
    ListByMonitorIDs(ctx context.Context, monitorIDs []int64) ([]*domain.MonitorCondition, error)
    DeleteKind(ctx context.Context, monitorID int64, kind string) error
    DeleteByMonitor(ctx context.Context, monitorID int64) error
}

// Similar interfaces for: NotificationRepository, StatusPageRepository,
// TagRepository, MaintenanceRepository, APIKeyRepository, IncidentRepository,
// UserRepository, SettingRepository, ProxyRepository, DockerHostRepository,
// TLSInfoRepository
```

```go
// internal/core/ports/checker.go
package ports

import "context"

type CheckResult struct {
    Status     domain.Status
    LatencyMs  int64 // primary connect/ping/select latency
    DurationMs int64 // complete check, including optional auxiliary queries
    Message    string
    Metadata   map[string]string  // e.g. {"tls_days_remaining": "45"}
    Conditions []domain.ConditionObservation
}

type Checker interface {
    Type() string
    Validate(config map[string]any) error
    Check(ctx context.Context, config map[string]any) (CheckResult, error)
}
```

```go
// internal/core/ports/notifier.go
package ports

import "context"
import "github.com/fiztoz/uptime-phoenix/internal/core/domain"

type NotificationSender interface {
    Type() string
    Validate(config map[string]any) error
    Send(ctx context.Context, config map[string]any, alert domain.AlertContext) error
}
```

```go
// internal/core/ports/eventbus.go
package ports

import "context"

type Event struct {
    Type    string
    Payload any
}

type EventBus interface {
    Publish(ctx context.Context, event Event) error
    Subscribe(eventType string) <-chan Event
    Close()
}
```

```go
// internal/core/ports/auth.go
package ports

import "context"
import "github.com/fiztoz/uptime-phoenix/internal/core/domain"

type Authenticator interface {
    Login(ctx context.Context, username, password string) (token string, err error)
    VerifyToken(ctx context.Context, token string) (userID int64, err error)
    HashPassword(password string) (string, error)
    VerifyPassword(hashed, password string) error
}

type TwoFactor interface {
    GenerateSecret() (secret string, qrURL string, err error)
    VerifyToken(secret, token string) bool
}
```

---

## 6. Database Schema (MariaDB)

### Design Notes

- **MariaDB 10.11+** (or 11.x) as the primary engine.
- **SQLite** for local dev and single-node edge (via `modernc.org/sqlite`, CGO-free).
- **Bun** as the query builder — supports both MariaDB and SQLite from the same Go code.
- **JSON columns** for per-type monitor config and per-provider notification config (MariaDB `JSON` type; SQLite stores as TEXT).
- **Partitioning** by RANGE on `time` for the `heartbeats` table (monthly partitions).
- **Migrations** via `bun/migrate` — versioned `.sql` files, embedded in binary.
  MariaDB and SQLite stay lockstep through `029_monitor_conditions`; every
  schema change has matching up/down files for both adapters.

### Core Tables

```sql
-- 001_init.up.sql (MariaDB variant)

-- users
CREATE TABLE users (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    username        VARCHAR(255) NOT NULL UNIQUE,
    password_hash   VARCHAR(255) NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    timezone        VARCHAR(150) DEFAULT 'UTC',
    totp_secret     VARBINARY(128),          -- encrypted TOTP secret
    totp_enabled    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
);

-- monitors (per-type config in JSON column)
CREATE TABLE monitors (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id         BIGINT,
    name            VARCHAR(255) NOT NULL,
    description     TEXT,
    type            VARCHAR(30) NOT NULL,    -- http, tcp, ping, dns, ...
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    interval        INT NOT NULL DEFAULT 60,
    retry_interval  INT NOT NULL DEFAULT 0,
    max_retries     INT NOT NULL DEFAULT 0,
    timeout         DOUBLE NOT NULL DEFAULT 30.0,
    parent_id       BIGINT,
    weight          INT NOT NULL DEFAULT 2000,
    push_token      VARCHAR(64),
    proxy_id        BIGINT,
    tls_ignore      BOOLEAN NOT NULL DEFAULT FALSE,
    accepted_statuscodes JSON NOT NULL DEFAULT '["200-299"]',
    resend_interval INT NOT NULL DEFAULT 0,
    upside_down     BOOLEAN NOT NULL DEFAULT FALSE,
    config          JSON NOT NULL,           -- per-type config
    docker_host_id  BIGINT,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL,
    FOREIGN KEY (parent_id) REFERENCES monitors(id) ON DELETE SET NULL,
    FOREIGN KEY (proxy_id) REFERENCES proxies(id) ON DELETE SET NULL,
    FOREIGN KEY (docker_host_id) REFERENCES docker_hosts(id) ON DELETE SET NULL,
    INDEX idx_monitors_active (active),
    INDEX idx_monitors_type (type),
    INDEX idx_monitors_user (user_id)
);

-- heartbeats (PARTITIONED by RANGE on time, monthly)
CREATE TABLE heartbeats (
    id          BIGINT NOT NULL AUTO_INCREMENT,
    monitor_id  BIGINT NOT NULL,
    status      TINYINT NOT NULL,            -- 0=DOWN, 1=UP, 2=PENDING, 3=MAINTENANCE
    time        TIMESTAMP NOT NULL,
    msg         TEXT,
    ping        INT,                         -- latency ms
    duration    INT NOT NULL DEFAULT 0,
    important   BOOLEAN NOT NULL DEFAULT FALSE,
    down_count  INT NOT NULL DEFAULT 0,
    PRIMARY KEY (id, time),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    INDEX idx_hb_monitor_time (monitor_id, time DESC)
) PARTITION BY RANGE (UNIX_TIMESTAMP(time)) (
    PARTITION p202606 VALUES LESS THAN (UNIX_TIMESTAMP('2026-07-01 00:00:00')),
    PARTITION p202607 VALUES LESS THAN (UNIX_TIMESTAMP('2026-08-01 00:00:00')),
    PARTITION p202608 VALUES LESS THAN (UNIX_TIMESTAMP('2026-09-01 00:00:00')),
    PARTITION p202609 VALUES LESS THAN (UNIX_TIMESTAMP('2026-10-01 00:00:00')),
    PARTITION p202610 VALUES LESS THAN (UNIX_TIMESTAMP('2026-11-01 00:00:00')),
    PARTITION p202611 VALUES LESS THAN (UNIX_TIMESTAMP('2026-12-01 00:00:00')),
    PARTITION p202612 VALUES LESS THAN (UNIX_TIMESTAMP('2027-01-01 00:00:00')),
    PARTITION pmax    VALUES LESS THAN MAXVALUE
);

-- latest auxiliary state, not heartbeat history (migration 029)
CREATE TABLE monitor_conditions (
    monitor_id           BIGINT NOT NULL,
    kind                 VARCHAR(32) NOT NULL,
    state                VARCHAR(16) NOT NULL,
    used_value           DOUBLE,
    limit_value          DOUBLE,
    percent_value        DOUBLE,
    threshold_value      DOUBLE,
    unit                 VARCHAR(24) NOT NULL DEFAULT '',
    resource             VARCHAR(32) NOT NULL DEFAULT '',
    scope                VARCHAR(32) NOT NULL DEFAULT '',
    source               VARCHAR(160) NOT NULL DEFAULT '',
    message              TEXT NOT NULL,
    observed_at          DATETIME(6) NOT NULL,
    stale_after          DATETIME(6) NOT NULL,
    last_success_at      DATETIME(6),
    consecutive_state    VARCHAR(16) NOT NULL DEFAULT '',
    consecutive_count    INT NOT NULL DEFAULT 0,
    last_notified_state  VARCHAR(16) NOT NULL DEFAULT '',
    last_notified_at     DATETIME(6),
    PRIMARY KEY (monitor_id, kind),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    INDEX idx_monitor_conditions_state (state, stale_after)
);

-- Aggregate rollup tables (same structure for 1m, 1h, 1d)
CREATE TABLE heartbeat_1m (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    monitor_id      BIGINT NOT NULL,
    bucket          TIMESTAMP NOT NULL,      -- truncated to minute
    up_count        INT NOT NULL DEFAULT 0,
    down_count      INT NOT NULL DEFAULT 0,
    pending_count   INT NOT NULL DEFAULT 0,
    maint_count     INT NOT NULL DEFAULT 0,
    avg_ping        DOUBLE,
    min_ping        INT,
    max_ping        INT,
    total_checks    INT NOT NULL DEFAULT 0,
    UNIQUE KEY uq_monitor_bucket (monitor_id, bucket),
    INDEX idx_1m_monitor_bucket (monitor_id, bucket DESC)
);

-- heartbeat_1h and heartbeat_1d follow the same pattern
-- (bucket truncated to hour / day respectively; heartbeat_1d uses DATE column)

-- reusable message layouts (migration 027)
CREATE TABLE notification_templates (
    id             BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id        BIGINT,
    name           VARCHAR(255) NOT NULL,
    provider       VARCHAR(50) NOT NULL,
    title_template VARCHAR(1000) NOT NULL DEFAULT '',
    body_template  TEXT NOT NULL,
    created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL
);

-- notifications (per-provider config in JSON)
CREATE TABLE notifications (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id     BIGINT,
    name        VARCHAR(255) NOT NULL,
    type        VARCHAR(50) NOT NULL,        -- telegram, discord, slack, ...
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    is_default  BOOLEAN NOT NULL DEFAULT FALSE,
    template_id BIGINT,
    config      JSON NOT NULL,               -- per-provider config
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL,
    FOREIGN KEY (template_id) REFERENCES notification_templates(id) ON DELETE SET NULL,
    INDEX idx_notif_user (user_id),
    INDEX idx_notif_type (type)
);

-- monitor_notification (many-to-many)
CREATE TABLE monitor_notification (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    monitor_id      BIGINT NOT NULL,
    notification_id BIGINT NOT NULL,
    UNIQUE KEY uq_pair (monitor_id, notification_id),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (notification_id) REFERENCES notifications(id) ON DELETE CASCADE
);

-- status_pages
CREATE TABLE status_pages (
    id                  BIGINT AUTO_INCREMENT PRIMARY KEY,
    slug                VARCHAR(255) NOT NULL UNIQUE,
    title               VARCHAR(255) NOT NULL,
    description         TEXT,
    icon                VARCHAR(255),
    theme               VARCHAR(30) NOT NULL DEFAULT 'light',
    published           BOOLEAN NOT NULL DEFAULT TRUE,
    custom_domain       VARCHAR(255),
    password_hash       VARCHAR(255),
    footer_text         TEXT,
    custom_css          TEXT,
    dashboard_style     VARCHAR(30) NOT NULL DEFAULT 'full',
    show_tags           BOOLEAN NOT NULL DEFAULT FALSE,
    auto_resolve_incidents BOOLEAN NOT NULL DEFAULT FALSE,
    sla_target          DECIMAL(6,3),
    created_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_sp_domain (custom_domain)
);

-- status_page_cnames (custom domain aliases)
CREATE TABLE status_page_cnames (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    status_page_id  BIGINT NOT NULL,
    domain          VARCHAR(255) NOT NULL UNIQUE,
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE
);

-- status_page_monitors
CREATE TABLE status_page_monitors (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    status_page_id  BIGINT NOT NULL,
    monitor_id      BIGINT NOT NULL,
    display_order   INT NOT NULL DEFAULT 1000,
    UNIQUE KEY uq_pair (status_page_id, monitor_id),
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);

-- tags
CREATE TABLE tags (
    id      BIGINT AUTO_INCREMENT PRIMARY KEY,
    name    VARCHAR(255) NOT NULL UNIQUE,
    color   VARCHAR(7) NOT NULL DEFAULT '#666666'
);

-- monitor_tags
CREATE TABLE monitor_tags (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    monitor_id  BIGINT NOT NULL,
    tag_id      BIGINT NOT NULL,
    value       TEXT,
    UNIQUE KEY uq_pair (monitor_id, tag_id),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (tag_id) REFERENCES tags(id) ON DELETE CASCADE
);

-- proxies
CREATE TABLE proxies (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id     BIGINT NOT NULL,
    protocol    VARCHAR(10) NOT NULL,        -- http, https, socks5
    host        VARCHAR(255) NOT NULL,
    port        INT NOT NULL,
    auth        BOOLEAN NOT NULL DEFAULT FALSE,
    username    VARCHAR(255),
    password    VARCHAR(255),                -- encrypted at app layer
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    is_default  BOOLEAN NOT NULL DEFAULT FALSE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

-- maintenance_windows
CREATE TABLE maintenance_windows (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id     BIGINT,
    title       VARCHAR(255) NOT NULL,
    description TEXT NOT NULL,
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    strategy    VARCHAR(50) NOT NULL DEFAULT 'single',  -- single, cron
    start_date  TIMESTAMP,
    end_date    TIMESTAMP,
    cron_expr   VARCHAR(100),
    duration    INT,                         -- minutes (for cron strategy)
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL
);

-- maintenance_window_monitors
CREATE TABLE maintenance_window_monitors (
    id                  BIGINT AUTO_INCREMENT PRIMARY KEY,
    maintenance_window_id BIGINT NOT NULL,
    monitor_id          BIGINT NOT NULL,
    UNIQUE KEY uq_pair (maintenance_window_id, monitor_id),
    FOREIGN KEY (maintenance_window_id) REFERENCES maintenance_windows(id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);

-- api_keys
CREATE TABLE api_keys (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id     BIGINT NOT NULL,
    name        VARCHAR(255) NOT NULL,
    key_hash    VARCHAR(255) NOT NULL UNIQUE,  -- SHA-256 of actual key
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    expires_at  TIMESTAMP,
    scopes      JSON NOT NULL DEFAULT '["read"]',
    last_used_at TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

-- incidents
CREATE TABLE incidents (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    status_page_id  BIGINT NOT NULL,
    title           VARCHAR(255) NOT NULL,
    content         TEXT NOT NULL,
    style           VARCHAR(30) NOT NULL DEFAULT 'warning',
    pinned          BOOLEAN NOT NULL DEFAULT TRUE,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (status_page_id) REFERENCES status_pages(id) ON DELETE CASCADE
);

-- docker_hosts
CREATE TABLE docker_hosts (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id         BIGINT NOT NULL,
    name            VARCHAR(255) NOT NULL,
    docker_daemon   VARCHAR(255) NOT NULL,
    docker_type     VARCHAR(50) NOT NULL DEFAULT 'socket',
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

-- tls_info (cached TLS cert info per monitor)
CREATE TABLE tls_info (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    monitor_id  BIGINT NOT NULL UNIQUE,
    info_json   JSON NOT NULL,
    checked_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);

-- settings (key-value app config)
CREATE TABLE settings (
    id      BIGINT AUTO_INCREMENT PRIMARY KEY,
    key     VARCHAR(200) NOT NULL UNIQUE,
    value   JSON NOT NULL
);

-- notification_sent_history (rate limiting / dedup)
CREATE TABLE notification_sent_history (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    notification_id BIGINT NOT NULL,
    monitor_id      BIGINT NOT NULL,
    last_sent_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uq_pair (notification_id, monitor_id),
    FOREIGN KEY (notification_id) REFERENCES notifications(id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);
```

### Partition Management

A monthly cron job (or application-scheduled task) runs:

```sql
ALTER TABLE heartbeats ADD PARTITION (
    PARTITION p202701 VALUES LESS THAN (UNIX_TIMESTAMP('2027-02-01 00:00:00'))
);
-- And drops partitions older than retention period:
ALTER TABLE heartbeats DROP PARTITION p202506;
```

### Common Queries

```sql
-- Latest heartbeat per monitor (for dashboard)
SELECT m.*, h.status, h.time, h.ping
FROM monitors m
LEFT JOIN heartbeats h ON h.monitor_id = m.id
  AND h.id = (
    SELECT id FROM heartbeats
    WHERE monitor_id = m.id
    ORDER BY time DESC LIMIT 1
  )
WHERE m.active = TRUE;

-- 24h chart data (from 1m rollup)
SELECT bucket, up_count, down_count, avg_ping
FROM heartbeat_1m
WHERE monitor_id = ? AND bucket >= NOW() - INTERVAL 24 HOUR
ORDER BY bucket ASC;

-- 30d uptime percentage (from 1h rollup)
SELECT
  SUM(up_count) / NULLIF(SUM(total_checks) - SUM(maint_count), 0) * 100 AS uptime_pct
FROM heartbeat_1h
WHERE monitor_id = ? AND bucket >= NOW() - INTERVAL 30 DAY;
```

---

## 7. Monitor Types (Checker Adapters)

Each monitor type is a single file implementing `ports.Checker`. The registry auto-discovers them.

### Checker Registry

```go
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
    Register(WebSocketChecker{})
    Register(PushChecker{})
    Register(DockerChecker{})
    Register(MQTTChecker{})
    Register(RabbitMQChecker{})
    Register(GRPCChecker{})
    Register(SNMPChecker{})
    Register(DatabaseChecker{})
    Register(S3Checker{})
}
```

### Monitor Types Summary

| Type | Library | Key Config Fields | Gotchas |
|---|---|---|---|
| `http` | `net/http` + `tidwall/gjson` | `url`, `method`, `headers`, `body`, `keyword`, `json_query`, `json_query_syntax`, `json_operator`, `expected_value`, `accepted_statuscodes` | Query syntax: native `gjson` (default) or the directly translatable JSONPath subset (`$`, child names, array indexes, `[*]`). JSON operators: `exists`, `has_value`, `not_exists`, `equals`, `not_equals`, `contains`, `not_contains`; `has_value` rejects `null`, `""`, `[]`, and `{}` but accepts `false` and `0`; set `context.WithTimeout`; extract TLS cert from `resp.TLS` |
| `tcp` | `net` (stdlib) | `hostname`, `port` | `net.DialTimeout` includes DNS resolution |
| `ping` | `prometheus-community/pro-bing` | `hostname`, `count` | Linux: `sysctl net.ipv4.ping_group_range`; macOS: unprivileged works |
| `dns` | `miekg/dns` | `hostname`, `resolve_type`, `resolve_server`, `expected_value` | Set `Client.Dialer.Timeout` |
| `websocket` | `coder/websocket` | `url`, `headers` | `HandshakeTimeout` critical; no default |
| `push` | `net/http` (inbound) | `push_token` (generated) | HMAC signature verification on `X-Signature` header |
| `docker` | `moby/moby/client` | `docker_daemon`, `container` | Local Unix socket or remote Docker API (`tcp://host:port`); see `docs/guides/docker-remote-setup.md` |
| `mqtt` | `eclipse/paho.mqtt.golang` | `broker`, `topic`, `username`, `password`, `success_message` | `SetAutoReconnect(false)` for one-shot check; check tokens |
| `rabbitmq` | `github.com/rabbitmq/amqp091-go` | `url` (aliases: `connection_string`, `dsn`), optional `queue`, `exchange`, `exchange_type` | AMQP 0-9-1 only; opens a channel; queue/exchange checks use passive declare; use least-privilege RabbitMQ user |
| `grpc` | `google.golang.org/grpc` + `health/v1` | `url`, `service_name`, `tls` | Use `grpc.WithBlock()` + context deadline |
| `snmp` | `gosnmp/gosnmp` | `hostname`, `oid`, `version`, `community` | Pure Go, no CGO; SNMPv3 needs extra fields |
| `database` | `pgx` / `mysql` / `go-mssqldb` / `mongo-driver/v2` / `go-redis/v9` | `engine`, `connection_string` (alias `dsn`), `health_check` (`ping`\|`select_1`), `check_session_pool`, `session_pool_threshold`, `check_storage`, `storage_scope` (`database`\|`instance`), `storage_threshold`, `storage_max_gb` | Engines: postgres, mysql, mariadb, mssql, mongodb, redis. Fixed presets only — no free-form SQL. Primary connect/ping/select drives UP/DOWN. Optional capacity queries run on the same cadence as the primary probe and emit persisted `ok`/`warning`/`error` conditions and derived `stale` without changing availability. `storage_scope=instance` sums all non-template (PostgreSQL) or visible (MySQL) databases on the instance; other engines keep their existing measurement. Extra grants may be required. Setup: `docs/guides/database-monitor-setup.md`; semantics: `docs/guides/database-capacity-presentation.md`. |
| `s3` | stdlib `net/http` + in-tree SigV4 | `provider`, `endpoint`, `region`, `bucket`, `path_style`, `access_key`, `secret_key`, `session_token`, `health_check` (`head_bucket`\|`head_object`\|`get_object`), `object_key` | Health only. Bucket names may contain `-` and `_`; `_` is not a DNS label and forces path-style. No usage/quota probe (S3 API has no cheap size call). Redirects are not followed. Setup: `docs/guides/s3-monitor-setup.md`. |

### K8s extensions hook (not a monitor type)

Operator-registered sidebar tabs that iframe a same-host Ingress path. This is
**not** a 14th checker and carries no plugin client or credentials.

`PHOENIX_EXTENSIONS` is a JSON array of `{id, title, path, icon}`. Empty or unset
serves `GET /api/extensions` as `[]` (any authenticated user; no extra RBAC).
`icon` is a same-origin path the plugin image serves (default `{path}/icon.svg`);
Helm cannot extract files from a container. Helm-only keys (`image`,
`secretName`, credentials) are never echoed. The chart loops `extensions[]`
into a Deployment, Service, NetworkPolicy, and an Ingress Prefix path
inserted before `/`. Default is off; Compose / single-binary / SQLite-laptop
still boot and get an empty list.

### Example: HTTP Checker

```go
// internal/adapters/checker/http.go
package checker

import (
    "context"
    "fmt"
    "io"
    "net/http"
    "strings"
    "time"
    "github.com/tidwall/gjson"
    "github.com/fiztoz/uptime-phoenix/internal/core/domain"
    "github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type HTTPChecker struct{}

func (HTTPChecker) Type() string { return "http" }

func (HTTPChecker) Validate(c map[string]any) error {
    if c["url"] == nil || c["url"] == "" {
        return fmt.Errorf("url is required")
    }
    return nil
}

func (HTTPChecker) Check(ctx context.Context, c map[string]any) (ports.CheckResult, error) {
    url := c["url"].(string)
    method := "GET"
    if m, ok := c["method"].(string); ok && m != "" { method = m }

    timeout := time.Duration(c["timeout"].(float64)) * time.Second
    ctx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()

    req, err := http.NewRequestWithContext(ctx, method, url, nil)
    if err != nil {
        return ports.CheckResult{Status: domain.StatusDown, Message: err.Error()}, nil
    }

    // Apply headers, auth, body from config...

    start := time.Now()
    resp, err := http.DefaultClient.Do(req)
    latency := time.Since(start).Milliseconds()

    if err != nil {
        return ports.CheckResult{Status: domain.StatusDown, LatencyMs: latency, Message: err.Error()}, nil
    }
    defer resp.Body.Close()

    body, _ := io.ReadAll(resp.Body)

    // 1. Status code assertion
    if !isAcceptedStatus(resp.StatusCode, c["accepted_statuscodes"]) {
        return ports.CheckResult{Status: domain.StatusDown, LatencyMs: latency,
            Message: fmt.Sprintf("unexpected status %d", resp.StatusCode)}, nil
    }

    // 2. Keyword assertion
    if kw, ok := c["keyword"].(string); ok && kw != "" {
        if !strings.Contains(string(body), kw) {
            return ports.CheckResult{Status: domain.StatusDown, LatencyMs: latency,
                Message: "keyword not found"}, nil
        }
    }

    // 3. JSON query assertion
    if jq, ok := c["json_query"].(string); ok && jq != "" {
        result := gjson.Get(string(body), jq)
        if !result.Exists() {
            return ports.CheckResult{Status: domain.StatusDown, LatencyMs: latency,
                Message: "json query path not found"}, nil
        }
    }

    // 4. TLS cert info extraction
    metadata := map[string]string{}
    if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
        cert := resp.TLS.PeerCertificates[0]
        daysLeft := int(time.Until(cert.NotAfter).Hours() / 24)
        metadata["tls_days_remaining"] = fmt.Sprintf("%d", daysLeft)
        metadata["tls_issuer"] = cert.Issuer.CommonName
    }

    return ports.CheckResult{Status: domain.StatusUp, LatencyMs: latency,
        Message: "OK", Metadata: metadata}, nil
}
```

---

## 8. Notification Providers (Sender Adapters)

Each notification provider is a single file implementing `ports.NotificationSender`.

### Registry

```go
// internal/adapters/notifier/registry.go
package notifier

import "github.com/fiztoz/uptime-phoenix/internal/core/ports"

var registry = map[string]ports.NotificationSender{}

func Register(s ports.NotificationSender) { registry[s.Type()] = s }
func Get(t string) (ports.NotificationSender, bool) { s, ok := registry[t]; return s, ok }

func init() {
    Register(TelegramSender{})
    Register(DiscordSender{})
    Register(SlackSender{})
    Register(SMTPSender{})
    Register(WebhookSender{})
    Register(TeamsSender{})
    Register(MattermostSender{})
    Register(GotifySender{})
    Register(BarkSender{})
    Register(FeishuSender{})
    Register(LineSender{})
}
```

Actual registration is driven by per-file `init()` + `registry.go` (same one-file plugin convention as checkers). Do **not** reintroduce ntfy/Pushover/PagerDuty/OpsGenie without explicit approval — they are not in the tree.

### Reusable Message Templates

Discord, SMTP, Webhook, and LINE can select an install-wide reusable message
template. `notifications.template_id` is nullable and uses `ON DELETE SET NULL`,
so deleting a template restores the provider's built-in layout instead of
breaking delivery. The provider on a template is immutable after creation, and
`NotificationService` rejects cross-provider assignments.

Templates use Uptime Phoenix placeholders such as `{{ monitor.name }}`, `{{ status }}`,
`{{ message }}`, `{{ check_output }}`, `{{ timestamp }}`, and `{{ ack_url }}`.
Generic `alert.*` placeholders resolve against either a monitor or a group;
explicit `monitor.*` and `group.*` placeholders expose scope-specific metadata.
`{{ status.emoji }}`, `{{ started_at.unix }}`, and `{{ timestamp.unix }}` support
Discord-native titles and `<t:UNIX:F>` / `<t:UNIX:R>` timestamp markup.
Webhook layouts may prefix any variable with `json.` (for example
`{{ json.message }}`) to emit a JSON-encoded value. The renderer rejects unknown
or malformed placeholders before persistence. `NotificationService` resolves
the selected template per delivery and passes it through `AlertContext`; only
the four supported sender adapters render it. Notifications with no template
retain their existing provider-specific output exactly.

Optional lifecycle variables fail empty rather than inventing values. Monitor
status alerts take `started_at` and `duration` from the persisted alert row, so
DOWN resends and recovery messages retain the original outage start; monitor
`tags` are resolved through `TagService`. Groups have no persisted outage
lifecycle or tags, so their lifecycle/tag variables are empty. `ack_url` is
present only for monitor DOWN alerts when `PUBLIC_URL` is configured; group and
recovery alerts leave it empty. Plain rendering emits an empty string for an
unknown optional value, while the corresponding `json.*` timestamp emits null.

Discord templates additionally persist a structured embed configuration in the
`notification_templates.config` JSON column (migration `028`). Operators can
customize the title link, footer, timestamp, status-specific colors, and up to
25 ordered fields. Field names and values are templates; a field whose rendered
name or value is empty is omitted, allowing monitor-only fields such as Target
and group-only fields such as Condition to coexist in one reusable layout.
Rendered payloads enforce Discord's per-field and 6,000-character aggregate
limits before sending.

SMTP templates use the same provider-specific `config` column without another
schema migration. A missing SMTP config means `plain`, preserving templates
created before HTML email support. HTML mode stores an additional
`html_body_template` while `body_template` remains the required plain-text
fallback. The SMTP adapter renders variables context-safely, sends both bodies
as `multipart/alternative`, and keeps the existing CR/LF stripping and length
limit on the subject. The admin composer previews the result inside a sandboxed
email frame; its content-security policy blocks scripts, forms, navigation, and
remote resources so previewing operator-authored markup cannot execute it in
Uptime Phoenix or leak the operator's address to tracking pixels.

### Severity Mapping

Shared formatting lives in `alert_format.go`. Providers map Uptime Phoenix status onto channel-native fields (examples below; see each `*_test.go` for exact payloads).

| Uptime Phoenix Status | Telegram / text | Discord embed | Slack | Webhook JSON | Teams / Mattermost / Gotify / Bark / Feishu / Line |
|---|---|---|---|---|---|
| UP (resolve) | normal text | color `0x00FF00` | `:white_check_mark:` | `severity: "UP"` | channel-specific success styling |
| DOWN (alert) | ⚠️ in text | color `0xFF0000` | `:x:` + `danger` | `severity: "DOWN"` | channel-specific alert styling |
| PENDING | normal | color `0xFFA500` | `:warning:` | `severity: "PENDING"` | warning styling where supported |
| MAINTENANCE | quiet | color `0x808080` | `:tools:` | `severity: "MAINTENANCE"` | muted / suppressed styling |

### Shared Rate-Limit Middleware

```go
// internal/adapters/notifier/ratelimit.go
package notifier

import (
    "net/http"
    "strconv"
    "time"
)

type RateLimitInfo struct {
    RateLimited bool
    RetryAfter  time.Duration
}

func extractRateLimit(resp *http.Response) RateLimitInfo {
    info := RateLimitInfo{}
    if resp.StatusCode == 429 {
        info.RateLimited = true
        if ra := resp.Header.Get("Retry-After"); ra != "" {
            if secs, err := strconv.Atoi(ra); err == nil {
                info.RetryAfter = time.Duration(secs) * time.Second
            }
        }
    }
    return info
}
```

### Example: Telegram Sender

```go
// internal/adapters/notifier/telegram.go
package notifier

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "github.com/fiztoz/uptime-phoenix/internal/core/domain"
    "github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type TelegramSender struct{}

func (TelegramSender) Type() string { return "telegram" }

func (TelegramSender) Validate(c map[string]any) error {
    if c["bot_token"] == nil || c["chat_id"] == nil {
        return fmt.Errorf("bot_token and chat_id are required")
    }
    return nil
}

func (TelegramSender) Send(ctx context.Context, c map[string]any, a domain.AlertContext) error {
    botToken := c["bot_token"].(string)
    chatID := c["chat_id"].(string)

    emoji := "✅"
    if a.Status == domain.StatusDown { emoji = "🔴" }
    if a.Status == domain.StatusMaintenance { emoji = "🔧" }

    text := fmt.Sprintf("%s *%s* is *%s*\n%s", emoji, a.MonitorName, a.Status, a.Message)

    body, _ := json.Marshal(map[string]any{
        "chat_id":    chatID,
        "text":       text,
        "parse_mode": "Markdown",
    })

    url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
    req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
    req.Header.Set("Content-Type", "application/json")

    resp, err := http.DefaultClient.Do(req)
    if err != nil { return err }
    defer resp.Body.Close()

    if rl := extractRateLimit(resp); rl.RateLimited {
        time.Sleep(rl.RetryAfter)
        // retry once...
    }

    if resp.StatusCode != 200 {
        return fmt.Errorf("telegram: unexpected status %d", resp.StatusCode)
    }
    return nil
}
```

---

## 9. Real-time Layer (WebSocket + EventBus)

### EventBus (Port + Adapters)

```go
// internal/adapters/eventbus/memory.go (Phase 1 default)
package eventbus

import "github.com/fiztoz/uptime-phoenix/internal/core/ports"

type MemoryBus struct {
    subscribers map[string][]chan ports.Event
}

func NewMemoryBus() *MemoryBus {
    return &MemoryBus{subscribers: make(map[string][]chan ports.Event)}
}

func (b *MemoryBus) Publish(ctx context.Context, event ports.Event) error {
    for _, ch := range b.subscribers[event.Type] {
        select {
        case ch <- event:
        default: // drop if subscriber is slow
        }
    }
    return nil
}

func (b *MemoryBus) Subscribe(eventType string) <-chan ports.Event {
    ch := make(chan ports.Event, 100)
    b.subscribers[eventType] = append(b.subscribers[eventType], ch)
    return ch
}
```

```go
// internal/adapters/eventbus/redis.go (Phase 2 opt-in)
// Uses go-redis/v9 PubSub; selected when REDIS_URL env is set
```

### WebSocket Hub

```go
// internal/adapters/ws/hub.go
package ws

import (
    "context"
    "sync"
    "github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type Hub struct {
    mu      sync.RWMutex
    clients map[*Client]bool
    bus     ports.EventBus
}

func NewHub(bus ports.EventBus) *Hub {
    h := &Hub{
        clients: make(map[*Client]bool),
        bus:     bus,
    }
    go h.listen()  // subscribe to EventBus, fan-out to clients
    return h
}

func (h *Hub) listen() {
    // Subscribe to all event types
    heartbeatCh := h.bus.Subscribe("heartbeat")
    statusCh := h.bus.Subscribe("status.change")
    monitorCh := h.bus.Subscribe("monitor.update")

    for {
        select {
        case ev := <-heartbeatCh:
            h.broadcast(ev)
        case ev := <-statusCh:
            h.broadcast(ev)
        case ev := <-monitorCh:
            h.broadcast(ev)
        }
    }
}

func (h *Hub) broadcast(event ports.Event) {
    h.mu.RLock()
    defer h.mu.RUnlock()
    for client := range h.clients {
        client.send(event)
    }
}
```

### Event Types

| Event | Payload | Trigger |
|---|---|---|
| `heartbeat` | `{monitor_id, status, time, ping, msg}` | Every monitor check completes |
| `status.change` | `{monitor_id, old_status, new_status, duration}` | Monitor transitions UP↔DOWN |
| `condition.update` | `{monitor_id, kind, state, used, limit, percent, threshold, unit, resource, scope, source, message, observed_at, stale_after}` | Latest auxiliary condition is persisted or its notification cursor changes |
| `condition.delete` | `{monitor_id, kind}` | A capacity check is disabled and live clients must remove its latest condition |
| `monitor.update` | `{monitor}` | Monitor config changed via CRUD |
| `monitor.list` | `{monitors: [...]}` | Initial rehydration on WS connect |
| `incident.create` | `{incident}` | New incident on status page |
| `incident.resolve` | `{id}` | Incident resolved |

### Client Connection Lifecycle

1. Client opens `wss://host/ws` with JWT in query param or header
2. Server validates JWT, creates `Client`, adds to `Hub`
3. Server emits `monitor.list` with full state for rehydration
4. Client subscribes to events; Svelte 5 runes update the DOM
5. On disconnect: client removed from Hub; reconnection logic in frontend retries

---

## 10. Frontend Architecture (Svelte 5)

### Route Groups

```
src/routes/
├── (admin)/               # SPA mode — ssr=false, csr=true
│   ├── +layout.ts         # export const ssr = false;
│   ├── dashboard/
│   ├── monitors/
│   ├── settings/
│   └── incidents/
├── (public)/              # SSG mode — prerender=true
│   ├── [domain]/
│   │   └── +page.ts       # export const prerender = true;
│   └── [domain]/history/
└── api/
    └── ws/                # WebSocket upgrade (if using SvelteKit server)
```

### Runes-based WebSocket Store

```typescript
// web/src/lib/stores/ws.svelte.ts

type WsStatus = 'connecting' | 'connected' | 'disconnected' | 'reconnecting';

type WsEvent = {
  type: string;
  payload: any;
};

function createWsStore(url: string) {
  let status = $state<WsStatus>('disconnected');
  let monitors = $state<Monitor[]>([]);
  let heartbeats = $state<Map<number, Heartbeat>>(new Map());
  let reconnectAttempt = $state(0);

  let isConnected = $derived(status === 'connected');

  let ws: WebSocket | null = null;
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null;

  function connect() {
    status = 'connecting';
    ws = new WebSocket(url);

    ws.onopen = () => {
      status = 'connected';
      reconnectAttempt = 0;
    };

    ws.onmessage = (e) => {
      const event: WsEvent = JSON.parse(e.data);
      handleEvent(event);
    };

    ws.onclose = (e) => {
      status = 'disconnected';
      if (e.code !== 1000) scheduleReconnect();
    };
  }

  function scheduleReconnect() {
    status = 'reconnecting';
    const delay = Math.min(1000 * 2 ** reconnectAttempt, 30_000);
    reconnectTimer = setTimeout(() => {
      reconnectAttempt++;
      connect();
    }, delay);
  }

  function handleEvent(event: WsEvent) {
    switch (event.type) {
      case 'monitor.list':
        monitors = event.payload;
        break;
      case 'heartbeat':
        heartbeats.set(event.payload.monitor_id, event.payload);
        heartbeats = new Map(heartbeats); // trigger reactivity
        break;
      case 'status.change':
        // update monitor status
        break;
    }
  }

  return {
    get status() { return status; },
    get isConnected() { return isConnected; },
    get monitors() { return monitors; },
    get heartbeats() { return heartbeats; },
    connect,
  };
}

export const realtime = createWsStore(`${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/ws`);
```

### Component Example: Dashboard

```svelte
<!-- web/src/routes/(admin)/dashboard/+page.svelte -->
<script lang="ts">
  import { realtime } from '$lib/stores/ws.svelte';
  import { onMount } from 'svelte';
  import MonitorCard from '$lib/components/MonitorCard.svelte';

  onMount(() => realtime.connect());
</script>

{#if !realtime.isConnected}
  <div class="banner disconnected">Reconnecting…</div>
{/if}

<div class="grid">
  {#each realtime.monitors as monitor (monitor.id)}
    <MonitorCard {monitor} heartbeat={realtime.heartbeats.get(monitor.id)} />
  {/each}
</div>
```

### Tech Stack

| Concern | Library | Why |
|---|---|---|
| Framework | Svelte 5 + SvelteKit | Runes = fine-grained reactivity; no VDOM |
| Build | Vite 5 | Standard |
| Adapter | `@sveltejs/adapter-node` | Prerender status pages + SPA admin in one build |
| i18n | `inlang/paraglide-js` v2 | Type-safe, tree-shakable, no runtime |
| UI primitives | `shadcn-svelte` (wraps `bits-ui`) | Owned, accessible, Tailwind-styled |
| Icons | `lucide-svelte` | Tree-shakable |
| Toasts | `svelte-sonner` | Modern, no flash |
| Forms | `sveltekit-superforms` + `zod` | Type-safe validation |
| Charts | `LayerCake` + D3 scales | Svelte-native, reactive, tiny (~8 KB) |
| CSS | Tailwind CSS v4 | Utility-first, consistent |

---

## 11. Authentication & 2FA

### Flow

1. **Login:** `POST /api/auth/login` with username + password → returns JWT
2. **2FA challenge (if enabled):** `POST /api/auth/verify-2fa` with TOTP token → returns JWT
3. **Passkey login:** `POST /api/auth/webauthn/login/{begin,finish}` → returns JWT
4. **OIDC SSO (opt-in):** `GET /api/auth/oidc/login` → IdP → `GET /api/auth/oidc/callback`
   → redirect to SPA with session JWT in the URL fragment. Identities are keyed by
   immutable `(issuer, subject)` (migration `019`). Group claims map onto existing
   `is_admin`, capability flags, and scoped grants — see service tests under
   `internal/core/services/auth_oidc*_test.go` (local agent contracts:
   `docs/local/F5-S13-OIDC-CONTRACTS.md`, gitignored).
   Local password + TOTP/passkey remain available for break-glass when OIDC is on.
5. **JWT verification:** Echo middleware validates JWT on every `/api/*` and `/ws` request
6. **API keys:** `GET /metrics` and external API access use API keys (hashed in DB, shown once at creation)

### Libraries

| Concern | Library |
|---|---|
| Password hashing | `golang.org/x/crypto/bcrypt` |
| JWT | `golang-jwt/jwt/v5` |
| TOTP | `pquerna/otp/totp` |
| WebAuthn / passkeys | `go-webauthn/webauthn` |
| OIDC SSO | `coreos/go-oidc/v3` + `golang.org/x/oauth2` |
| API key | Custom (SHA-256 hash, constant-time compare) |

### Auth Port Interface

```go
type Authenticator interface {
    Login(ctx context.Context, username, password string) (token string, err error)
    VerifyToken(ctx context.Context, token string) (userID int64, err error)
    HashPassword(password string) (string, error)
    VerifyPassword(hashed, password string) error
    IssueSession(ctx context.Context, userID int64) (token string, err error)
    IssuePending2FATicket(ctx context.Context, userID int64) (ticket string, err error)
    VerifyPending2FATicket(ctx context.Context, ticket string) (userID int64, err error)
}

type TwoFactor interface {
    GenerateSecret(issuer, username string) (secret string, qrURL string, err error)
    VerifyToken(secret, token string) bool
}

// OIDCAuthenticator — discovery, Auth Code exchange, ID-token validation.
// OIDCIdentityRepository — (issuer, subject) → user_id links.
// See internal/core/ports/oidc.go and auth_oidc*_test.go.
```

---

## 12. Status Pages

### Architecture

- **SvelteKit SSG** — status pages are prerendered at build time for SEO and speed
- **Runtime hydration** — the prerendered HTML hydrates with live data via a public REST endpoint (`GET /api/status/:slug`)
- **Custom domain routing** — `hooks.server.ts` resolves the incoming hostname to a status page slug

```typescript
// web/src/hooks.server.ts
import type { Handle } from '@sveltejs/kit';

export const handle: Handle = async ({ event, resolve }) => {
  const hostname = event.url.hostname;

  // Check if hostname matches a custom status page domain
  if (hostname !== 'admin.phoenix.io' && hostname !== 'localhost') {
    const slug = await resolveDomainToSlug(hostname); // calls /api/status/resolve?domain=...
    if (slug) {
      event.url.pathname = `/${slug}${event.url.pathname}`;
    }
  }

  return resolve(event);
};
```

### Status Page Features

- Multiple status pages per install
- Custom domain + CNAME aliases
- Incident management (create, pin, resolve, auto-resolve)
- Public status pages render monitors before a compact active-only incident
  section. Resolved incidents remain in the authenticated incident history;
  admins may permanently delete them only after resolution.
- 90-day uptime bar (LayerCake)
- Public `/{slug}/history` page with newest-first 12-month and 4-quarter component tables.
  Summaries come from UTC `heartbeat_1d` rollups; planned maintenance is excluded and periods
  without effective checks serialize `uptime_percent: null` rather than fabricated uptime.
- Optional per-page SLA target (`status_pages.sla_target`, migration `018`). The public history
  page compares measured periods only when configured; `0` on update clears the target.
- Optional password protection through the `PasswordHasher` port (8–72-byte
  access codes, five verification attempts per client/page per minute; Redis
  shares the limiter across API pods when configured)
- Unknown uptime is serialized as `null`; the public UI never converts missing
  observations or repository failures into 100% uptime
- Custom CSS + footer text
- Light/dark theme
- Per-page dashboard style (`full`, `grid`, or compact `pills`)
- Multi-language (paraglide-js)
- **Email subscriptions (Sprint C):** double opt-in via purpose-bound JWTs
  (`SubscriberTokenCodec`), one active SMTP notification channel per page
  (`status_page_subscription_channels`), confirmation + unsubscribe tokens (no
  plaintext secrets at rest). Requires absolute `PUBLIC_URL`. Legacy webhook
  subscriber rows live in `status_page_subscribers_legacy_webhook` after
  migration `014`.
- **Public TLS expiry:** when `TLSInfo` is cached, public monitor payloads may
  include `cert_expiry_date` / `cert_days_left` (never invented zeros).
- **Server-rendered OG/unfurls:** embedded Go SPA fallback injects escaped
  per-page Open Graph / Twitter tags into `index.html` only (`status_meta.go`);
  assets remain byte-identical. Adapter-static `hooks.server.ts` is not used by
  the embedded binary.

### Certificate-expiry alerts (Sprint C)

- Opt-in per monitor: `cert_expiry_notify` (default false; migration `013`).
- HTTP checker emits exact `tls_not_after` (RFC3339) plus days/issuer.
- `CertificateAlertService` runs in the owning heartbeat worker (not EventBus)
  and alerts once when the cert first enters the most urgent of 30/14/7 days;
  last threshold + NotAfter are persisted inside `tls_info.info_json`.
- Maintenance windows suppress send **and** do not mark the threshold sent.
- `AlertContext.EventKind = certificate_expiry` so all 11 notifiers render
  cert-specific copy (webhook payload includes event kind + cert fields).

### Database capacity conditions

- Primary availability stays four-state: DOWN / UP / PENDING / MAINTENANCE.
  Connect/auth/ping/fixed `SELECT 1` decide it. Capacity never introduces a
  fifth heartbeat status.
- Optional fixed engine queries emit typed `session_pool` and `storage`
  observations only after the primary probe succeeds. Query or privilege
  failures become condition `error`, never a silent skip and never fake DOWN.
- `MonitorConditionService` persists the latest state in migration `029`.
  `state` is the confirmed/stable value; `consecutive_state`/`count` is the
  candidate. Warning/error/recovery promote after two consecutive candidate
  samples. Recovery hysteresis is evaluated against the stable warning state
  (an 80% warning stays latched until two samples below 75%). The first-ever
  warning/error is unconfirmed until the second sample. Capacity queries run
  on every primary check. The UI derives `stale` after three monitor intervals
  (minimum three minutes).
- `CheckResult.LatencyMs` ends after primary ping/select;
  `CheckResult.DurationMs` includes capacity queries. Heartbeat `ping` and
  `duration` therefore retain distinct meanings.
- `AlertContext.EventKind = capacity_condition`. All 11 providers render
  warning/error/recovery copy; webhook and reusable templates expose
  `condition.kind`, `condition.state`, `condition.previous_state`, measurements,
  semantic labels, source, and observation time.
- Maintenance suppresses delivery without advancing the cursor. Two normal
  samples emit recovery. One coarse per-condition delivery cursor is persisted;
  a per-channel ledger is a documented follow-up.
- `GET /api/monitor-conditions` and `condition.update` / `condition.delete`
  frames are scoped through
  `AccessService`. REST/WS use explicit snake-case views and never expose
  lifecycle cursor fields.
- Capacity chips feed dashboard Needs attention and monitor detail only.
  Confirmed warning/error/stale conditions count as attention on active
  monitors; paused and maintenance monitors do not. Availability Insights,
  uptime/SLA, badges, folders, and public status pages remain
  reachability-only. Unconfirmed first-ever warning/error rows stay off
  REST and WebSocket until the second sample.

### Maintenance timezone (Sprint C)

- `MaintenanceWindow.Timezone` is an IANA name (default/empty → UTC).
- Cron windows evaluate via `CronEvaluator.IsWindowActive(..., loc)` in that
  location; fixed single windows remain absolute UTC instants.
- Active interval is half-open `[start, end)`.

### Adoption tooling

- **`cmd/kuma-import`:** read-only converter from Uptime Kuma **SQLite file** or
  **MariaDB/MySQL DSN** (Kuma v2) → Uptime Phoenix `BackupDocument` JSON → admin backup
  import. See `docs/KUMA-IMPORT.md`.
- **Release dry-run / publish:** local via `scripts/release/dry-run.sh` and CI via
  `.github/workflows/release.yml` (dry-run always; publish owner-gated). See
  `docs/RELEASING.md`. PR/main gate is `.github/workflows/ci.yml` (restored
  2026-07-28). `LICENSE` (MIT) is present.

---

## 13. Scheduler & Heartbeat Lifecycle

### Scheduler (Phase 1 — in-process)

```go
// internal/adapters/scheduler/local.go
package scheduler

import (
    "context"
    "time"
    "github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type LocalScheduler struct {
    monitorRepo ports.MonitorRepository
    checkerRegistry func(string) (ports.Checker, bool)
    heartbeatSvc *services.HeartbeatService
    clock ports.Clock
}

func (s *LocalScheduler) Run(ctx context.Context) {
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            s.tick(ctx)
        }
    }
}

func (s *LocalScheduler) tick(ctx context.Context) {
    monitors, _ := s.monitorRepo.ListActive(ctx)
    now := s.clock.Now()

    for _, m := range monitors {
        // Check if it's time to run this monitor
        if !s.shouldRun(m, now) { continue }

        go s.runCheck(ctx, m)  // concurrent per-monitor
    }
}

func (s *LocalScheduler) runCheck(ctx context.Context, m *domain.Monitor) {
    checker, ok := s.checkerRegistry(m.Type)
    if !ok { return }

    result, err := checker.Check(ctx, m.Config)
    if err != nil {
        result = ports.CheckResult{Status: domain.StatusDown, Message: err.Error()}
    }

    // Apply upside-down mode
    if m.UpsideDown {
        if result.Status == domain.StatusUp { result.Status = domain.StatusDown }
        if result.Status == domain.StatusDown { result.Status = domain.StatusUp }
    }

    // Save heartbeat, publish event, evaluate status change
    s.heartbeatSvc.Record(ctx, m, result)
}
```

### Heartbeat Service

```go
// internal/core/services/heartbeat_service.go
func (s *HeartbeatService) Record(ctx context.Context, monitor *domain.Monitor, result ports.CheckResult) error {
    hb := &domain.Heartbeat{
        MonitorID: monitor.ID,
        Status:    result.Status,
        Time:      s.clock.Now(),
        Msg:       result.Message,
        Ping:      int(result.LatencyMs),
        Duration:  int(result.DurationMs),
    }

    // 1. Save heartbeat
    if err := s.heartbeats.Save(ctx, hb); err != nil { return err }

    // 2. Publish heartbeat event (WS clients get it)
    s.bus.Publish(ctx, ports.Event{Type: "heartbeat", Payload: hb})

    // 3. Check for status transition
    latest, _ := s.heartbeats.GetLatest(ctx, monitor.ID)
    if latest != nil && latest.Status != result.Status {
        s.bus.Publish(ctx, ports.Event{
            Type: "status.change",
            Payload: map[string]any{
                "monitor_id":  monitor.ID,
                "old_status":  latest.Status,
                "new_status":  result.Status,
            },
        })

        // 4. Fire notifications on transition
        s.notificationSvc.Notify(ctx, monitor, result.Status, latest.Status)
    }

    // 5. Save TLS info if available
    if days, ok := result.Metadata["tls_days_remaining"]; ok {
        s.tlsInfoRepo.Save(ctx, monitor.ID, result.Metadata)
    }

    // 6. Persist and evaluate auxiliary conditions independently. This does
    // not rewrite hb.Status and failures never suppress the saved heartbeat.
    s.conditionSvc.OnCheck(ctx, monitor, result.Conditions)

    return nil
}
```

The real service fetches the previous heartbeat before saving (with the
deterministic `time DESC, id DESC` tie-break), applies retry/PENDING semantics,
then runs TLS and condition side channels in the owning worker. Status-change
notifications and condition notifications therefore cannot be duplicated by
Redis EventBus fan-out.

### Aggregate Rollup

A periodic job (every minute) computes `heartbeat_1m` from raw heartbeats:

```sql
INSERT INTO heartbeat_1m (monitor_id, bucket, up_count, down_count, ...)
SELECT
    monitor_id,
    DATE_FORMAT(time, '%Y-%m-%d %H:%i:00') AS bucket,
    SUM(CASE WHEN status = 1 THEN 1 ELSE 0 END) AS up_count,
    SUM(CASE WHEN status = 0 THEN 1 ELSE 0 END) AS down_count,
    AVG(ping) AS avg_ping,
    COUNT(*) AS total_checks
FROM heartbeats
WHERE time >= NOW() - INTERVAL 2 MINUTE AND time < NOW() - INTERVAL 1 MINUTE
GROUP BY monitor_id, DATE_FORMAT(time, '%Y-%m-%d %H:%i:00')
ON DUPLICATE KEY UPDATE
    up_count = VALUES(up_count),
    down_count = VALUES(down_count),
    ...;
```

---

## 14. Observability

### Metrics (`/metrics` endpoint)

Exposed via `prometheus/client_golang`, behind API-key Basic Auth:

| Metric | Type | Labels |
|---|---|---|
| `phoenix_monitor_status` | Gauge | `monitor_id`, `monitor_name`, `type` |
| `phoenix_monitor_latency_ms` | Gauge | `monitor_id`, `monitor_name` |
| `phoenix_heartbeats_total` | Counter | `monitor_id`, `status` |
| `phoenix_notifications_sent_total` | Counter | `provider`, `status` |
| `phoenix_ws_connections_active` | Gauge | — (used by HPA) |
| `phoenix_monitors_active` | Gauge | — |

### Logging

`log/slog` JSON to stdout. Every log entry includes: `time`, `level`, `msg`, `module`, `request_id` (for HTTP), `monitor_id` (for checks).

### Health Endpoints

| Endpoint | Purpose | K8s Probe |
|---|---|---|
| `GET /api/health/live` | Process is alive | livenessProbe |
| `GET /api/health/ready` | DB connected + can serve | readinessProbe |

### Tracing (Phase 3)

OpenTelemetry SDK spans: API → WS → worker → checker → notification. Exported to Jaeger/Tempo via OTLP.

---

## 15. Deployment (K8s + Helm)

### Helm Chart Structure

```
charts/uptime-phoenix/
├── Chart.yaml
├── Chart.lock              # pinned optional subchart dependencies
├── charts/
│   └── valkey-*.tgz       # vendored official Valkey subchart
├── values.yaml              # default: single-pod, MariaDB on PVC, embedded frontend
└── templates/
    ├── deployment.yaml      # single pod by default; split when scaling.mode=multi
    ├── service.yaml
    ├── ingress.yaml         # TLS, WS-friendly timeouts
    ├── pvc.yaml             # 10Gi for MariaDB data
    ├── secret.yaml          # DB password, JWT key
    ├── configmap.yaml       # non-secret config
    ├── pdb.yaml             # minAvailable: 1
    ├── _helpers.tpl
    └── tests/
        └── test-connection.yaml
```

### Default `values.yaml`

```yaml
# Default: minimal-dependency, single pod
image:
  repository: ghcr.io/fiztoz/uptime-phoenix
  tag: ""               # empty → Chart.AppVersion
  pullPolicy: IfNotPresent

# Single pod by default
scaling:
  mode: single          # single | multi | sharded

# MariaDB on PVC (default). Set enabled=false to use external.
mariadb:
  enabled: true
  persistence:
    size: 10Gi
  rootPassword: ""       # auto-generated if empty

# External MariaDB (when mariadb.enabled=false)
mariadbExternal:
  host: ""
  port: 3306
  database: phoenix
  username: phoenix
  password: ""

# External Redis EventBus (opt-in, only needed for multi-pod modes)
redis:
  enabled: false
  existingSecret: ""       # full redis:// or rediss:// URL
  existingSecretKey: redis-url
  host: ""
  port: 6379
  password: ""

# Official Valkey subchart in this release (also opt-in)
valkey:
  enabled: false
  auth:
    enabled: true
    managedSecret: true
    password: ""           # generated and retained when empty
    usersExistingSecret: "{{ .Release.Name }}-vk-auth"
    aclUsers:
      default:
        permissions: "~* &* +@all"
        passwordKey: default
  dataStorage:
    enabled: true
    requestedSize: 1Gi
  replica:
    enabled: false
    replicas: 2
    persistence:
      size: 1Gi
  networkPolicy:
    ingress:
      - from:
          - podSelector: {}

# Frontend delivery
web:
  split: false           # false = embedded in Go binary; true = separate Deployment

# Ingress
ingress:
  enabled: true
  className: nginx
  host: phoenix.example.com
  tls: true

# Resource limits
resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 1000m
    memory: 512Mi

# Security
podSecurityContext:
  runAsNonRoot: true
  runAsUser: 65532
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false

# PDB
podDisruptionBudget:
  minAvailable: 1
```

The optional in-release dependency is the project-owned official Valkey chart.
It uses the official Valkey image, is pinned by `Chart.lock`, and is vendored
into the parent chart so local rendering, packaged releases, and Argo CD do not
need to resolve a dependency at render time. Phoenix generates and retains the
default ACL user's password Secret and wires the Valkey primary Service into
`REDIS_URL`. Standalone and primary-plus-replica modes are supported. An
external Redis-compatible server remains available through `redis.enabled`,
with either host/port/password values or a Secret containing the complete URL.
The two paths are mutually exclusive and both remain disabled by default. GitOps
deployments use `valkey.auth.managedSecret=false` with a pre-created Secret
because offline manifest rendering cannot retain a password through `lookup`.

Config and secrets are injected as env (`configMapKeyRef` / `secretKeyRef`).
The API, worker, and all-in-one Deployments stamp the pod template with
`checksum/config` (hash of the rendered ConfigMap) and `checksum/secret` (hash
of the values that become Secrets — not the rendered Secret templates, which
call `lookup` + `randAlphaNum` and are unstable under Argo CD). Changing those
inputs changes the annotations, so the next Helm/Argo apply rolls the pods.
The optional web Deployment hashes the nginx ConfigMap; cloudflared hashes the
tunnel token. Secrets referenced only via `*.existingSecret` are outside this
mechanism.

### Deployment Commands

```bash
# Default install — zero external dependencies
helm install uptime-phoenix ./charts/uptime-phoenix

# With external MariaDB
helm install uptime-phoenix ./charts/uptime-phoenix \
  --set mariadb.enabled=false \
  --set mariadbExternal.host=mariadb.internal \
  --set mariadbExternal.password=secret

# Scale to multi-pod with the in-release official Valkey subchart
helm upgrade uptime-phoenix ./charts/uptime-phoenix \
  --set mode=split \
  --set database.engine=mariadb \
  --set mariadb.enabled=true \
  --set valkey.enabled=true \
  --set web.split=true

# Use SQLite instead of MariaDB (single-node edge)
helm install uptime-phoenix ./charts/uptime-phoenix \
  --set mariadb.enabled=false \
  --set database.engine=sqlite
```

### Dockerfile (Go binary, CGO-free, embedded frontend)

```dockerfile
# Stage 1: Build frontend
FROM node:22-alpine AS web-builder
WORKDIR /app/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Stage 2: Build Go binary
FROM golang:1.23-alpine AS go-builder
WORKDIR /app
COPY go.* ./
RUN go mod download
COPY . .
COPY --from=web-builder /app/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /phoenix ./cmd/app

# Stage 3: Final image (distroless, ~25 MB)
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=go-builder /phoenix /phoenix
EXPOSE 3000
USER 65532
ENTRYPOINT ["/uptime-phoenix"]
```

### Graceful Shutdown

```go
// cmd/app/main.go
ctx, stop := signal.NotifyContext(context.Background(),
    syscall.SIGINT, syscall.SIGTERM)
defer stop()

go func() {
    if err := e.Start(":3000"); err != nil && !errors.Is(err, http.ErrServerClosed) {
        log.Fatal(err)
    }
}()

<-ctx.Done()
log.Info("shutdown signal received")

// 1. Stop scheduler (no new checks)
// 2. Close WS connections gracefully
// 3. Flush pending events
// 4. Close DB pool
shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
e.Shutdown(shutdownCtx)
```

---

## Appendix: Go Dependency Manifest

```go.mod (selected)
module phoenix
go 1.23

require (
    github.com/labstack/echo/v4 v4.13.0       // HTTP framework
    github.com/coder/websocket v1.8.13         // WebSocket
    github.com/uptrace/bun v1.2.11             // SQL query builder (MariaDB + SQLite)
    github.com/go-sql-driver/mysql v1.9.0      // MariaDB driver
    modernc.org/sqlite v1.34.0                 // SQLite driver (CGO-free)
    github.com/redis/go-redis/v9 v9.7.0        // Redis (opt-in Phase 2)
    github.com/golang-jwt/jwt/v5 v5.2.1        // JWT auth
    golang.org/x/crypto/bcrypt                 // Password hashing
    github.com/pquerna/otp v1.4.0              // TOTP 2FA
    github.com/coreos/go-oidc/v3 v3.20.0       // OIDC discovery + ID token verify
    golang.org/x/oauth2 v0.36.0                // OIDC Authorization Code flow
    github.com/prometheus-community/pro-bing v0.4.0 // ICMP ping
    github.com/miekg/dns v1.1.63               // DNS queries
    github.com/eclipse/paho.mqtt.golang v1.5.0  // MQTT
    github.com/rabbitmq/amqp091-go v1.10.0       // RabbitMQ AMQP 0-9-1
    github.com/gosnmp/gosnmp v1.39.0           // SNMP
    github.com/docker/docker v27.3.1           // Docker API
    google.golang.org/grpc v1.67.1             // gRPC health
    github.com/tidwall/gjson v1.18.0           // JSON query for HTTP monitors
    github.com/prometheus/client_golang v1.20.0 // /metrics endpoint
    github.com/robfig/cron/v3 v3.0.1           // Cron scheduling
    github.com/caarlos0/env/v11 v11.3.0        // Config from env
    github.com/google/uuid v1.6.0              // UUID generation
    go.mongodb.org/mongo-driver/v2 v2.1.0      // MongoDB connect ping
    github.com/jackc/pgx/v5 v5.7.0             // PostgreSQL connect ping
    github.com/microsoft/go-mssqldb v1.10.0    // SQL Server (MSSQL) connect ping (CGO-free)
    gopkg.in/mail.v2 v2.3.1                    // SMTP email notifications
    github.com/nicksnyder/go-i18n/v2 v2.4.0    // (optional) server-side i18n
)
```

All libraries are **CGO-free** (except MongoDB's optional CSE, which we don't use). The final binary is a single static executable that cross-compiles for `linux/amd64`, `linux/arm64`, and `linux/arm/v7`.
