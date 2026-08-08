# Uptime-Kuma-Class Tool — K8s Architecture & Phased Rollout

> Companion to `uptime-kuma.md` and `uptime-kuma-stack-alternatives.md`
> Refines the plan given the requirements:
> 1. **Easy K8s deployment** is mandatory
> 2. **Frontend and backend must be separate** so the backend can scale independently
> 3. **Web app only** (no Tauri desktop) for the early phase
> 4. **TOTP only** for 2FA in the early phase (no WebAuthn/passkeys)

---

## 1. The Decision in One Paragraph

The stack stays **Go 1.23+ (backend) + Svelte 5 (frontend)**, but the deployment model follows a **minimal-dependency-first** principle: the default deployment is **one pod, one PVC, zero external services**. The Go binary serves the built Svelte assets via `embed.FS`, uses SQLite or MariaDB-on-PVC for storage, and runs an in-process EventBus. No Redis, no external DB, no separate web tier is required to function.

When scale demands it, the same binary is reconfigured via Helm values to run as multiple pods: the `EventBus` port swaps from in-process to Redis pub/sub, the frontend splits into a separate `uptime-phoenix-web` Deployment (nginx serving the static build), and MariaDB moves to a managed external instance. **No domain code changes** when any of these swaps happen — they are adapter changes selected at startup by environment variables.

---

## 2. High-Level Architecture (Phase 2 — what we're designing toward)

```
                              ┌────────────────────────────────┐
                              │  Browser / Mobile / CLI client │
                              │  (Svelte 5 SPA)               │
                              └───────────────┬────────────────┘
                                              │
                                              │ HTTPS + WSS
                                              │
                              ┌───────────────▼────────────────┐
                              │  Ingress Controller            │
                              │  (nginx / traefik + cert-mgr)  │
                              └───┬────────────────────────┬───┘
                                  │                        │
            /api/*  /ws/*         │                        │  /  /status/*
                                  │                        │
                ┌─────────────────▼──┐              ┌──────▼─────────┐
                │  uptime-phoenix-web       │              │  uptime-phoenix-web   │
                │  (SvelteKit +      │              │  (SvelteKit    │
                │   nginx)           │              │   status page  │
                │  replicas: 2-5     │              │   SSG build)   │
                │                    │              │  replicas: 2   │
                └────────────────────┘              └────────────────┘

                ┌────────────────────┐
                │  uptime-phoenix-api       │ ◀─── HPA: CPU + active WS connections
                │  (Go binary)       │
                │  replicas: 2-N     │
                │                    │
                │  ┌──────────────┐  │
                │  │ HTTP handlers│  │      publishes
                │  │  + WS hub    │ ─┼─────────────────────┐
                │  │              │  │                     │
                │  │ EventBus ◀───┼──┘                     │
                │  │ (Redis pub/  │                        │
                │  │  sub)        │                        │
                │  └──────────────┘  │                     │
                └─────────┬──────────┘                     │
                          │                                │
                          │                                │
              ┌───────────▼─────────┐         ┌───────────▼──────────┐
              │   PostgreSQL        │         │   Redis              │
              │   (StatefulSet or   │         │  - pub/sub fan-out   │
              │    external RDS)    │         │  - rate-limit cache  │
              │                     │         │  - session cache     │
              │  Source of truth    │         │  (NOT source of      │
              │                     │         │   truth)             │
              └─────────────────────┘         └──────────────────────┘

                          ▲
                          │  reads
                          │
                ┌─────────┴──────────┐
                │  uptime-phoenix-worker    │  (Phase 2: separated)
                │  (Go binary)       │  replicas=1 (or sharded N in Phase 3)
                │                    │
                │  Scheduler         │  ──── publishes ──▶ Redis
                │  + Checker pool    │
                │  + Notification    │  ──── reads ──▶ Postgres
                │    dispatcher      │
                └────────────────────┘
```

**Key K8s principles baked in:**

1. **Stateless API tier.** `uptime-phoenix-api` has no local state. Pods can be killed at any time; clients reconnect via `wss://` and the load balancer routes them to a healthy pod.
2. **Worker tier separated.** The actual monitor checks run in `uptime-phoenix-worker`, not in the API pods. API pods only serve user traffic and WS.
3. **EventBus is a port, not a library.** The domain code calls `bus.Publish(ctx, evt)`. In Phase 1 the implementation is in-process. In Phase 2 it's Redis pub/sub. In Phase 3 it could be NATS, Kafka, or anything else.
4. **Postgres is the only source of truth.** Redis is a best-effort cache and pub/sub layer. If Redis is down, the system still works (heartbeats are still written to Postgres); the UI just won't update in real time until Redis is back.
5. **SQLite only for dev/single-user.** In K8s, Postgres is the default. SQLite is fine for local dev and for a single-node "edge" deployment.

---

## 3. Phased Rollout (the part that makes the architecture safe to start small)

### 3.1 Phase 1 — Single Pod, Zero External Dependencies (weeks 1–8)

**Goal:** Uptime-Kuma feature parity. Single K8s Deployment. No Redis. No external DB. Embedded frontend.

```
┌────────────────────────────┐
│  phoenix                   │  single Deployment, single pod
│  (Go binary, all-in-one)   │
│                            │
│  ┌──────────────────────┐  │
│  │ HTTP server (Echo)   │  │
│  │  + WebSocket hub     │  │
│  │  + static assets     │  │  ← embedded via Go embed.FS
│  │    (Svelte build)    │  │
│  └──────────┬───────────┘  │
│             │              │
│  ┌──────────▼───────────┐  │
│  │ Scheduler + Checkers │  │
│  │ + Notif. Dispatcher  │  │
│  └──────────┬───────────┘  │
│             │              │
│  ┌──────────▼───────────┐  │
│  │ EventBus (in-process)│  │  ← no Redis needed
│  └──────────┬───────────┘  │
│             │              │
│  ┌──────────▼───────────┐  │
│  │ Repository           │  │
│  │  MariaDB or SQLite   │  │
│  └──────────────────────┘  │
└─────────────┬──────────────┘
              │
   ┌──────────▼──────────┐
   │  PVC (MariaDB data)  │  ← or SQLite file in same PVC
   │  10-20 Gi            │
   └─────────────────────┘
```

**Domain code:**
- `EventBus` is an interface in `core/ports/`
- The only implementation is `InProcessEventBus` in `adapters/eventbus/memory.go`
- All goroutines in one process, no Redis yet
- Frontend is embedded in the Go binary via `//go:embed web/dist`

**K8s manifests (default mode):**
- `Deployment/phoenix` (replicas=1, PDB `minAvailable: 1`)
- `Service/phoenix` (ClusterIP, port 3000)
- `Ingress` with TLS
- `PersistentVolumeClaim` (10-20 Gi for MariaDB/SQLite data)
- `Secret/phoenix-db` (root password, generated)
- `ConfigMap/phoenix-config`

**What "easy K8s deployment" means here:**
- `helm install uptime-phoenix ./charts/uptime-phoenix` brings up a working monitoring tool with **zero external dependencies**
- Default: single pod, MariaDB on PVC, embedded frontend
- No need to provision Postgres, Redis, or a separate web tier
- The Docker image is multi-arch and distroless (~25 MB)

**Effort:** ~8 weeks for a Go-fluent dev to reach Uptime Kuma feature parity.

### 3.2 Phase 2 — Split worker from API + Redis (weeks 8–14, OPT-IN)

**Trigger:** you start hitting the 10k-monitors wall on a single pod, OR you want rolling deploys of the UI without restarting the scheduler, OR you need HA (no single-point-of-failure).

**Change (all enabled via Helm values, not code changes):**
- `phoenix` splits into two binaries from the same Go module: `cmd/api/main.go` and `cmd/worker/main.go`
- `uptime-phoenix-api` (stateless, replicas=2-10, HPA) — HTTP, WS, status page rendering. Frontend can be embedded (default) or split to `uptime-phoenix-web` (opt-in via `--set web.split=true`)
- `uptime-phoenix-worker` (replicas=1, then N with sharding later) — scheduler + checkers + notification dispatcher
- Add Redis (StatefulSet or managed). The `EventBus` port gets a second implementation: `adapters/eventbus/redis.go`. Switch the binding in `main.go` based on `REDIS_URL` env presence.
- API pods subscribe to Redis topics; worker publishes; clients connected to any API pod see the events.
- MariaDB can stay on PVC (with the worker as the only writer) or move to a managed external instance via `--set mariadb.enabled=false --set mariadb.external.host=...`

**Domain code changes:** zero. The `bus.Publish(...)` calls in `core/services/heartbeat_service.go` already exist from Phase 1.

**Effort:** ~1–2 weeks (the Go split is mostly `main.go` reorg + Redis wiring + Helm chart updates).

### 3.3 Phase 3 — Sharded workers (weeks 14+)

**Trigger:** a single worker pod can't keep up with 50k+ monitors across all protocols.

**Change:**
- `uptime-phoenix-worker` now uses DB-leased sharding: each worker claims a hash range of `monitor.id` via Postgres advisory locks or a `worker_lease` table.
- Workers self-elect via `SELECT ... FOR UPDATE SKIP LOCKED` and renew the lease every 10s.
- Failed workers release their lease automatically (timeout).
- API pods still subscribe to Redis pub/sub as before; no change to the client side.
- HPA on the worker tier based on lease-saturation (custom metric) or CPU.

**Domain code changes:** zero. The scheduler is an adapter (`adapters/scheduler/db-sharded.go`); the previous `adapters/scheduler/local.go` still exists for single-pod.

**Effort:** ~1–2 weeks.

### 3.4 Why this path is safe

- **Hexagonal pays off here.** Each phase swap is a change to an adapter, not to the domain.
- **The client never changes.** Svelte 5 SPA connects to `wss://...` and rehydrates from any pod. If the pod dies, the browser reconnects in <1s and receives a fresh `info` event.
- **Rollback is trivial.** A bad deploy of `uptime-phoenix-api` just rolls back the Deployment; the worker keeps running and the events buffer in Redis briefly.
- **Postgres is the migration story.** Every state change is in Postgres. If you blow up the whole Redis cluster, you lose realtime updates for ~30s while the next heartbeat tick writes to Postgres and clients re-poll.

---

## 4. Frontend / Backend Separation — Embedded by Default, Split Opt-In

### 4.1 The principle

The default deployment serves the frontend **from inside the Go binary** via `//go:embed web/dist`. This is the zero-dependency path — no nginx, no separate Deployment, no CDN config needed. One pod, one image, one process.

Separating into a `uptime-phoenix-web` Deployment is an **opt-in** choice for when you need:
- Independent scaling of the frontend (e.g., status pages get traffic spikes, API doesn't)
- CDN edge delivery of static assets
- Different release cadence for UI vs API

Both modes ship in the same Helm chart. Toggle via `--set web.split=true`.

### 4.2 Mode A — Embedded (default, zero-dependency)

```go
// cmd/app/main.go (or cmd/api/main.go in split mode)
import (
    "embed"
    "net/http"
)

//go:embed web/dist/*
var webAssets embed.FS

func setupStaticRoutes(e *echo.Echo) {
    assetFS := http.FS(webAssets)
    fileServer := http.FileServer(assetFS)
    e.GET("/*", echo.WrapHandler(fileServer))
}
```

- Single Go binary contains the frontend
- No nginx, no separate image
- `helm install uptime-phoenix ./charts/uptime-phoenix` → one pod, works immediately
- Final Docker image: ~25 MB (Go binary with embedded assets, distroless base)

### 4.3 Mode B — Split (opt-in, for scale)

Enabled via `--set web.split=true`. Creates a separate `uptime-phoenix-web` Deployment.

**Image 1: `uptime-phoenix-api`** — Go binary, no embedded frontend. HTTP + WS + scheduler.

**Image 2: `uptime-phoenix-web`** — SvelteKit build served by nginx.
- Multi-stage Dockerfile: `node:22-alpine` builds → `nginx:1.27-alpine` serves
- Final size: ~5–8 MB (nginx + static files)

**Helm chart conditional templates:**
```yaml
{{- if .Values.web.split }}
# uptime-phoenix-web Deployment + Service
{{- end }}
```

The Ingress routes `/` to `uptime-phoenix-web` (split mode) or to `uptime-phoenix` (embedded mode) based on the same value.

### 4.5 HPA (opt-in, Phase 2 only)

```yaml
# charts/uptime-phoenix/templates/ingress.yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: phoenix
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"     # for WS
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
spec:
  tls:
    - hosts: [phoenix.example.com]
      secretName: phoenix-tls
  rules:
    - host: phoenix.example.com
      http:
        paths:
          - path: /api
            pathType: Prefix
            backend:
              service:
                name: uptime-phoenix-api   # or "uptime-phoenix" in embedded mode
                port:
                  number: 3000
          - path: /ws
            pathType: Prefix
            backend:
              service:
                name: uptime-phoenix-api
                port:
                  number: 3000
          - path: /
            pathType: Prefix
            backend:
              service:
                {{- if .Values.web.split }}
                name: uptime-phoenix-web
                port:
                  number: 80
                {{- else }}
                name: uptime-phoenix-api   # embedded mode: same pod serves /
                port:
                  number: 3000
                {{- end }}
```

```yaml
# Only created when scaling.mode=multi (Phase 2)
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: uptime-phoenix-api
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: uptime-phoenix-api
  minReplicas: 2
  maxReplicas: 10
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 70
    - type: Pods
      pods:
        metric:
          name: phoenix_ws_connections_active
        target:
          type: AverageValue
          averageValue: "200"
```

### 4.6 PDB (applies to all modes)

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: phoenix
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app: phoenix   # matches single-pod, api, and worker in all modes
```

### 4.7 Graceful shutdown (Go side)

```go
// cmd/api/main.go
ctx, stop := signal.NotifyContext(context.Background(),
    syscall.SIGINT, syscall.SIGTERM)
defer stop()

go func() {
    if err := e.Start(":3000"); err != nil && !errors.Is(err, http.ErrServerClosed) {
        log.Fatal(err)
    }
}()

<-ctx.Done()  // wait for SIGTERM
log.Info("api: shutdown signal received")

// 1. stop accepting new HTTP requests
// 2. close WebSocket connections gracefully
// 3. flush pending events to Redis
// 4. close DB connection pool
shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
e.Shutdown(shutdownCtx)
```

The `preStop: sleep 10` in the K8s manifest gives the load balancer time to drain traffic before the kubelet sends SIGTERM. This is the standard pattern.

---

## 5. Locked Decisions Summary

| # | Decision | Rationale |
| 1 | **Go 1.23+ + Svelte 5** | Go: monitoring ecosystem, ICMP native, single binary, 2× faster dev than Rust. Svelte 5: fine-grained reactivity for real-time, smallest bundles. |
| 2 | **Minimal-dependency default** | Single pod, embedded frontend, MariaDB on PVC. `helm install` → works. No Redis, no external DB, no separate web tier required. |
| 3 | **MariaDB primary, SQLite for dev/edge** | MariaDB on PVC for K8s; SQLite for local dev. Same repository interface, different adapter. |
| 4 | **Frontend embedded by default** | `//go:embed web/dist` in the Go binary. Split to `uptime-phoenix-web` Deployment is opt-in via `--set web.split=true`. |
| 5 | **TOTP only for early phase** | `pquerna/otp`. WebAuthn port reserved for later. |
| 6 | **Web app only (no Tauri)** | Tauri wrapper can be added later as a thin shell around the web build. |
| 7 | **K8s-native from day 1** | Helm chart ships in Phase 1. Scaling from single-pod to multi-pod is a Helm value change, not a code rewrite. |

---

## 6. Concrete Tech Stack (Final, minimal-dependency-first)

| Layer | Phase 1 (Single Pod, zero deps) | Phase 2 (Split, opt-in) | Phase 3 (Sharded, opt-in) |
|---|---|---|---|
| **Backend binary** | `cmd/app/main.go` | `cmd/api/main.go` + `cmd/worker/main.go` | same + `adapters/scheduler/db-sharded.go` |
| **HTTP** | Echo v4 | Echo v4 | Echo v4 |
| **WebSocket** | `coder/websocket` | `coder/websocket` | `coder/websocket` |
| **EventBus** | `adapters/eventbus/memory.go` (in-process) | `adapters/eventbus/redis.go` (opt-in) | `adapters/eventbus/redis.go` |
| **Database** | **MariaDB on PVC** (default) or SQLite (dev/edge) | MariaDB (PVC or external) | MariaDB (external, managed) |
| **ORM** | **Bun** (SQL-first, MariaDB + SQLite) | Bun | Bun |
| **DB drivers** | `go-sql-driver/mysql` (MariaDB) + `modernc.org/sqlite` (SQLite, CGO-free) | same | same |
| **Migrations** | `bun/migrate` (versioned SQL, MariaDB + SQLite compatible) | same | same |
| **Cache / pub-sub** | **none** (in-process) | Redis (opt-in) | Redis |
| **Auth** | bcrypt + jwt + pquerna/otp (TOTP) | same | same |
| **Scheduler** | in-process goroutine | in-worker goroutine | DB-leased sharding |
| **Metrics** | prometheus/client_golang → `/metrics` | same | same |
| **Health** | `/api/health/live`, `/api/health/ready` | same | same |
| **Logging** | log/slog (JSON) → stdout | same | same |
| **Config** | env (caarlos0/env) + ConfigMap | same | same |
| **Frontend delivery** | **Embedded in Go binary** (`//go:embed web/dist`) | Embedded (default) or separate `uptime-phoenix-web` Deployment (opt-in) | Separate `uptime-phoenix-web` (CDN-friendly) |
| **Docker base** | `gcr.io/distroless/static-debian12:nonroot` | same | same |
| **Docker size** | ~25 MB (single image, embedded frontend) | ~25 MB (api) + ~5 MB (web, if split) | same |
| **K8s objects (default)** | 1 Deployment, 1 Service, 1 Ingress, 1 PVC, 1 Secret, 1 ConfigMap, 1 PDB | + Redis StatefulSet (opt-in) + worker Deployment (opt-in) + HPA + web Deployment (opt-in) | + sharded worker logic |
| **Helm chart** | `charts/uptime-phoenix/` — single-pod mode by default | same chart, `--set scaling.mode=multi` | same chart, `--set scaling.mode=sharded` |
| **Frontend** | SvelteKit (SSG status pages + SPA admin) → embedded in Go binary | same build, served by Go or nginx | same |
| **Frontend i18n** | `inlang/paraglide-js` | same | same |
| **Frontend state** | Svelte 5 runes + native WebSocket | same | same |
| **Frontend build** | Vite 5 | same | same |

---

## 7. Migration / Day-1 Operations

- **First install:** `helm install uptime-phoenix oci://ghcr.io/fiztoz/charts/uptime-phoenix --set adminPassword=...` brings up a working monitoring tool with **zero external dependencies** in <2 minutes. Single pod, MariaDB on PVC, embedded frontend.
- **First admin user:** created via a one-time `Job` that runs `phoenix-admin init` against the DB.
- **Backup:** CronJob that runs `mariadb-dump` (or copies the SQLite file) to a PVC or S3.
- **Upgrade:** `helm upgrade uptime-phoenix ...` — Helm handles the rolling update, PDB ensures no downtime.
- **Scale up to multi-pod:** `helm upgrade uptime-phoenix ... --set scaling.mode=multi --set redis.enabled=true` — adds Redis StatefulSet, splits worker from API. No code changes.
- **Move MariaDB to managed external:** `helm upgrade ... --set mariadb.enabled=false --set mariadb.external.host=... --set mariadb.external.password=...`
- **Observability:** the `/metrics` endpoint feeds Prometheus → Grafana. Structured logs go to Loki via Promtail.
- **Multi-cluster / DR:** replicate MariaDB via streaming replication or use a managed MariaDB with point-in-time recovery. Redis is ephemeral and rebuildable.

---

## 8. Sources

- Kubernetes patterns for stateful apps: <https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/>
- HPA custom metrics: <https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/>
- Helm chart best practices: <https://helm.sh/docs/chart_best_practices/>
- WebSocket over nginx ingress: <https://kubernetes.github.io/ingress-nginx/examples/websocket/>
- `coder/websocket` graceful shutdown: <https://github.com/coder/websocket>
- Previous research: `research/uptime-kuma.md`, `research/uptime-kuma-stack-alternatives.md`
