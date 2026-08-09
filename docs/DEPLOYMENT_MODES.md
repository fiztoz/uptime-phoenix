# Uptime Phoenix — Deployment Modes (All-in-one vs Truly Split)

Uptime Phoenix ships as **one codebase that runs in two shapes**:

| | All-in-one (default) | Truly split |
|---|---|---|
| Processes | 1 (`MODE=all`) | 2+ (`MODE=api` + `MODE=worker`) |
| Frontend | Embedded in the Go binary | Separate nginx tier |
| EventBus | In-process (memory) | Redis (cross-process) |
| Database | SQLite **or** MariaDB | MariaDB (shared) |
| External deps | **Zero** | MariaDB + Redis |
| Best for | Dev, small/self-hosted | Scale-out, HA, independent tuning |

The split exists so the **API** (HTTP + WebSocket, stateless, scale-out) and the
**worker** (scheduler + checkers + notifier, stateful-ish) can be scaled and
operated independently. They are the same binary's behaviour selected by `MODE`:

- `cmd/app`    → `MODE` from env (default `all`)
- `cmd/api`    → hard-codes `MODE=api`   (HTTP + WS only)
- `cmd/worker` → hard-codes `MODE=worker` (scheduler/checkers/notifier, no HTTP)

> **Why Redis in split mode?** The worker records heartbeats and publishes events
> to the EventBus; the API's WebSocket hub subscribes to that bus and pushes live
> updates to browsers. In one process the in-memory bus connects them. Across two
> processes they only meet on a **shared Redis** bus — without it the dashboard
> stops updating live (data still lands in MariaDB, but no live push).

---

## 1. Local development

### Mode A — All-in-one (recommended for day-to-day dev)

Zero external services. SQLite, embedded UI, one process.

```bash
make run                      # build + run, http://localhost:3000
# login: admin / ChangeMe123!
```

Hot-reload (frontend on :5173 proxies /api and /ws to the backend on :3000):

```bash
make dev                      # backend (air) + frontend (vite) together
# or, in two terminals:
make dev-backend              # Go backend, :3000
make dev-frontend             # Vite dev server, :5173  → open this one
```

See [LOCAL_DEVELOPMENT.md](LOCAL_DEVELOPMENT.md) for the full single-process guide.

### Mode B — Truly split (API + worker + Redis + MariaDB + nginx)

Use this when you want to develop or reproduce the production split locally.
Everything is wired by `docker-compose.split.yml` and built by `Dockerfile.split`.

```bash
docker compose -f docker-compose.split.yml up --build
# open http://localhost:8080   (login: admin / ChangeMe123!)
```

What comes up:

| Service | Image target | Role | Port |
|---|---|---|---|
| `mariadb` | `mariadb:11` | Shared system of record | 3306 (internal) |
| `redis` | `redis:7-alpine` | Cross-process EventBus | 6379 (internal) |
| `uptime-phoenix-api` | `Dockerfile.split --target api` | HTTP + WebSocket, **runs migrations** | 3000 (also published for debugging) |
| `api-ready-gate` | `busybox` | One-shot; blocks the worker until the API has migrated | — |
| `uptime-phoenix-worker` | `Dockerfile.split --target worker` | Scheduler + checkers + notifier | none |
| `uptime-phoenix-web` | `Dockerfile.split --target web` | nginx: serves the SPA, proxies `/api` + `/ws` | **8080 → use this** |

Notes for developers:

- **Browse via the web tier on :8080.** The browser talks only to nginx, which
  proxies to the API — so it's same-origin and there is no CORS to configure.
- **The API owns migrations.** The migration runner is *not* concurrency-safe, so
  `api-ready-gate` holds the worker back until `uptime-phoenix-api` reports
  `/api/health/ready`. Keep this ordering if you add services.
- **Iterate on one tier:**
  ```bash
  docker compose -f docker-compose.split.yml up -d --build uptime-phoenix-worker
  docker compose -f docker-compose.split.yml logs -f uptime-phoenix-worker
  ```
- **Override secrets** via a `.env` next to the compose file:
  ```dotenv
  MARIADB_PASSWORD=...
  JWT_SECRET=...
  BOOTSTRAP_PASSWORD=...
  LOG_LEVEL=debug
  ```
- **Reset state:** `docker compose -f docker-compose.split.yml down -v` (the `-v`
  drops the MariaDB volume).

### Build the role images directly (CI / registry)

```bash
docker build -f Dockerfile.split --target api    -t uptime-phoenix-api:dev    .
docker build -f Dockerfile.split --target worker -t uptime-phoenix-worker:dev .
docker build -f Dockerfile.split --target web    -t uptime-phoenix-web:dev    .
```

The all-in-one image is still `docker build -t uptime-phoenix:dev .` (root `Dockerfile`).

---

## 2. Using the Helm chart in each mode

The chart is `charts/uptime-phoenix`. The `mode` value selects the shape; `helm install`
with no overrides gives the zero-dependency single pod.

> **Operator guides:** [Helm & Argo CD](guides/helm-and-argocd.md) ·
> [Docker / GHCR](guides/docker-ghcr.md) · [binaries](guides/binaries.md)

### Mode: `all` (default — single pod, SQLite, embedded UI)

```bash
helm install uptime-phoenix ./charts/uptime-phoenix
# 1 Deployment, 1 PVC (/data), embedded SPA, in-process EventBus, no external deps
```

MariaDB instead of SQLite (still one pod):

```bash
helm install uptime-phoenix ./charts/uptime-phoenix \
  --set database.engine=mariadb \
  --set mariadb.enabled=true
```

### Mode: `split` (truly split — API + worker in one release)

Renders **both** the API and worker Deployments, a Service that targets the API
pods, and an initContainer on the worker that waits for the API (migration
ordering). Requires a **shared MariaDB** and **Redis**.

```bash
helm install uptime-phoenix ./charts/uptime-phoenix \
  --set mode=split \
  --set database.engine=mariadb \
  --set database.persistence.enabled=false \
  --set mariadbExternal.host=mariadb.internal \
  --set mariadbExternal.username=phoenix \
  --set mariadbExternal.password=$MARIADB_PASSWORD \
  --set redis.enabled=true \
  --set redis.host=redis-master \
  --set api.replicas=3 \
  --set worker.replicas=1
```

What you get:

- `uptime-phoenix-api` Deployment — `MODE=api`, `api.replicas` pods, behind the Service
  and Ingress, scalable (see HPA below). **Owns DB migrations.**
- `uptime-phoenix-worker` Deployment — `MODE=worker`, no HTTP, `initContainer` waits for
  `uptime-phoenix-api:<port>/api/health/ready` before starting so migrations run once.
- `redis-url` Secret in `redis://[:password@]host:port/0` form (consumed by both
  tiers as the EventBus).

> **SQLite is not valid for split** — two pods can't share a SQLite file safely.
> Use `database.engine=mariadb` and set `database.persistence.enabled=false`
> (the API/worker pods are stateless; MariaDB holds the data).

#### Scaling the worker (sharding)

A single worker owns all monitors. To run multiple workers that split the load
via DB leases:

```bash
helm upgrade uptime-phoenix ./charts/uptime-phoenix --reuse-values \
  --set worker.replicas=3 \
  --set worker.shards.enabled=true     # sets WORKER_ID per pod + lease config
```

#### Scaling the API (HPA)

```bash
helm upgrade uptime-phoenix ./charts/uptime-phoenix --reuse-values \
  --set scaling.mode=multi             # enables the HPA template
# tune hpa.minReplicas / hpa.maxReplicas / hpa.cpuTargetAverageUtilization
```

### Modes: `api` / `worker` (single-component releases)

`mode=api` renders **only** the API tier; `mode=worker` renders **only** the
worker. Use these when you want each component in its **own** release/namespace
(e.g. different teams, separate rollout cadence) pointing at the same MariaDB +
Redis. For a single coherent release, prefer `mode=split` above.

```bash
helm install uptime-phoenix-api    ./charts/uptime-phoenix --set mode=api    --set redis.enabled=true ...
helm install uptime-phoenix-worker ./charts/uptime-phoenix --set mode=worker --set redis.enabled=true ...
```

### Expose via Cloudflare Tunnel (no external ingress)

Instead of (or alongside) a public Ingress controller / LoadBalancer, the chart
can run a built-in `cloudflared` sidecar Deployment that dials **outbound** to
Cloudflare and tunnels traffic to the in-cluster Uptime Phoenix Service. No inbound
ports are opened; Cloudflare terminates TLS at its edge.

It uses the **named-tunnel token model**: you create a named tunnel in the
Cloudflare Zero Trust dashboard, add a Public Hostname route pointing at the
phoenix Service (e.g. `http://<release>-api:3000`), and Cloudflare hands you a
single connector **token** that encodes the tunnel id, credentials, and config.
The chart templates that token into a Secret (or you reference an existing one)
and injects it as `TUNNEL_TOKEN`.

Because Cloudflare fronts the app, you normally disable the chart Ingress:

```bash
# Inline token (chart creates the Secret)
helm upgrade --install uptime-phoenix charts/uptime-phoenix \
  --set ingress.enabled=false \
  --set cloudflareTunnel.enabled=true \
  --set cloudflareTunnel.token='<connector-token>'

# Or reference a pre-created Secret
kubectl create secret generic my-cf-tunnel --from-literal=token='<connector-token>'
helm upgrade --install uptime-phoenix charts/uptime-phoenix \
  --set ingress.enabled=false \
  --set cloudflareTunnel.enabled=true \
  --set cloudflareTunnel.existingSecret=my-cf-tunnel
```

Defaults: 2 connector replicas (HA — Cloudflare load-balances across them),
pinned `cloudflare/cloudflared` image, liveness/readiness probes on cloudflared's
metrics endpoint (`:2000`). Set `cloudflareTunnel.extraArgs` for extra flags.
When `networkPolicy.enabled=true`, egress to the phoenix Service (3000/80) and to
Cloudflare's edge (443) is opened automatically.

### Verify before applying

```bash
helm lint charts/uptime-phoenix
helm template uptime-phoenix charts/uptime-phoenix --set mode=all
helm template uptime-phoenix charts/uptime-phoenix \
  --set mode=split --set database.engine=mariadb \
  --set mariadbExternal.host=db --set mariadbExternal.password=x \
  --set redis.enabled=true
```

---

## 3. Mode cheat-sheet

| I want to… | Do this |
|---|---|
| Hack on a feature fast | `make dev` (all-in-one, hot reload) |
| Run the whole thing locally, one process | `make run` |
| Reproduce the prod split locally | `docker compose -f docker-compose.split.yml up --build` → :8080 |
| Build role images for a registry | `docker build -f Dockerfile.split --target {api,worker,web} …` |
| Deploy single pod to K8s | `helm install uptime-phoenix ./charts/uptime-phoenix` |
| Deploy split to K8s | `helm install … --set mode=split --set database.engine=mariadb --set redis.enabled=true` |
| Scale API horizontally | `--set scaling.mode=multi` (HPA) |
| Scale workers | `--set worker.replicas=N --set worker.shards.enabled=true` |
| Expose without public ingress | `--set ingress.enabled=false --set cloudflareTunnel.enabled=true --set cloudflareTunnel.token=<token>` |
