# Uptime Phoenix Helm Chart

A self-hosted, K8s-native, minimal-dependency monitoring tool. The default `helm install` produces a single-pod deployment with SQLite, embedded frontend, and zero external services.

> **Install guides:** [Helm & Argo CD](../../docs/guides/helm-and-argocd.md) ·
> [Docker / GHCR](../../docs/guides/docker-ghcr.md) · [binaries](../../docs/guides/binaries.md)

## Quick Start

### From GHCR

```bash
helm upgrade --install uptime-phoenix \
  oci://ghcr.io/fiztoz/charts/uptime-phoenix \
  --version 0.3.4 \
  --namespace uptime-phoenix --create-namespace
```

### From a git checkout

```bash
helm install uptime-phoenix ./charts/uptime-phoenix
helm install uptime-phoenix ./charts/uptime-phoenix -f my-values.yaml
helm upgrade uptime-phoenix ./charts/uptime-phoenix
```

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `image.tag` | string | `""` (`.Chart.AppVersion`) | App image tag. Empty uses the chart's `appVersion` |
| `web.image.tag` | string | `""` (`.Chart.AppVersion`) | Split-web image tag. Empty uses the chart's `appVersion` |
| `database.engine` | string | `sqlite` | `sqlite` (default, zero-dep) or `mariadb` |
| `database.pool.maxOpenConns` | int | `10` | MariaDB max open connections **per process** (`DB_MAX_OPEN_CONNS`; ignored for SQLite) |
| `database.pool.maxIdleConns` | int | `2` | MariaDB idle connections kept in the pool (`DB_MAX_IDLE_CONNS`) |
| `database.pool.connMaxIdleSeconds` | int | `30` | Close idle MariaDB connections after this many seconds (`DB_CONN_MAX_IDLE_SECONDS`) |
| `database.pool.connMaxLifetimeSeconds` | int | `300` | Recycle MariaDB connections after this many seconds (`DB_CONN_MAX_LIFETIME_SECONDS`) |
| `database.persistence.enabled` | bool | `true` | Enable PVC for `/data` (SQLite DB file) |
| `database.persistence.size` | string | `1Gi` | PVC size for data |
| `mariadb.enabled` | bool | `false` | Enable MariaDB mode (requires external or additional setup) |
| `scaling.mode` | string | `single` | `single` \| `multi` \| `sharded` |
| `hpa.enabled` | bool | `false` | Create the API HPA (CPU via metrics-server). Ignored when `mode=worker`. |
| `hpa.wsConnections.enabled` | bool | `false` | Add `phoenix_ws_connections_active` (needs `custom.metrics.k8s.io` / prometheus-adapter) |
| `redis.enabled` | bool | `false` | Connect Phoenix to an external Redis-compatible server |
| `redis.existingSecret` | string | `""` | Secret containing a complete `redis://` or `rediss://` URL |
| `redis.existingSecretKey` | string | `redis-url` | Key containing the URL in `redis.existingSecret` |
| `redis.host` | string | `""` | External Redis hostname when no existing Secret is used |
| `redis.port` | int | `6379` | External Redis port |
| `redis.password` | string | `""` | External Redis password |
| `valkey.enabled` | bool | `false` | Deploy the official Valkey subchart and wire Phoenix to it |
| `valkey.auth.enabled` | bool | `true` | Enable Valkey ACL authentication |
| `valkey.auth.managedSecret` | bool | `true` | Generate and retain the Valkey password Secret |
| `valkey.dataStorage.requestedSize` | string | `1Gi` | Standalone Valkey PVC size |
| `valkey.replica.enabled` | bool | `false` | Enable a primary plus persistent Valkey replicas |
| `web.split` | bool | `false` | Split frontend to separate Deployment (opt-in) |
| `ingress.enabled` | bool | `true` | Enable Ingress with nginx WS timeout annotations |
| `cloudflareTunnel.enabled` | bool | `false` | Run a `cloudflared` Deployment to expose Uptime Phoenix via a Cloudflare named tunnel (no inbound ingress) |
| `cloudflareTunnel.image.repository` | string | `cloudflare/cloudflared` | cloudflared image |
| `cloudflareTunnel.image.tag` | string | `2024.12.2` | Pinned cloudflared tag |
| `cloudflareTunnel.replicas` | int | `2` | Connector replicas (2 = HA; Cloudflare load-balances) |
| `cloudflareTunnel.token` | string | `""` | Inline connector token (templated into a Secret by the chart) |
| `cloudflareTunnel.existingSecret` | string | `""` | Reference an existing Secret holding the token instead of `token` |
| `cloudflareTunnel.secretKey` | string | `token` | Key within the Secret holding the token |
| `cloudflareTunnel.extraArgs` | list | `[]` | Extra flags for `cloudflared tunnel run` |
| `cloudflareTunnel.resources` | object | requests/limits | CPU/memory for the connector |
| `resources` | object | requests/limits | CPU/memory requests and limits |
| `podSecurityContext` | object | nonroot + readOnlyRootFS | Security settings (enforced) |
| `config.logLevel` | string | `info` | Log level |
| `config.host` | string | `0.0.0.0` | Listen host |
| `config.port` | int | `3000` | Listen port |
| `config.jwtExpireHours` | int | `24` | JWT expiration |
| `secret.jwt` | string | `""` | Stable signing key supplied directly through protected values; mutually exclusive with `secret.existingSecret` |
| `secret.existingSecret` | string | `""` | Stable pre-created JWT signing Secret; safer alternative to an inline value for GitOps flows |
| `secret.existingSecretKey` | string | `jwt-secret` | Key within `secret.existingSecret` |
| `config.totpIssuer` | string | `Phoenix` | TOTP issuer name |
| `extensions` | list | `[]` | Optional iframe plugins. Empty = no extra Deployments, catalogue is `[]` |
| `extensions[].id` | string | — | DNS-1123 label (required) |
| `extensions[].title` | string | — | Sidebar label (required) |
| `extensions[].path` | string | — | Ingress path + iframe `src`; must start with `/` and must not collide with Phoenix routes |
| `extensions[].image` | string | — | Operator-supplied plugin image (required) |
| `extensions[].pullPolicy` | string | `IfNotPresent` | Image pull policy |
| `extensions[].port` | int | `80` | Container / Service port |
| `extensions[].replicas` | int | `1` | Replica count |
| `extensions[].resources` | object | `{}` | Optional CPU/memory |
| `extensions[].env` | list | `[]` | Extra env vars |
| `extensions[].envFromSecret` | string | `""` | Optional Secret; all keys injected as env |
| `extensions[].database.secretName` | string | `""` | Optional Secret mounted as `DATABASE_DSN` (never Phoenix's `DB_DSN`) |
| `extensions[].database.secretKey` | string | `dsn` | Key within `database.secretName` |

Env vars exposed to container: `DB_ENGINE`, `DB_DSN`, `JWT_SECRET` (auto-generated for Helm CLI, supplied by `secret.jwt`, or read from `secret.existingSecret`), `JWT_EXPIRE_HOURS`, `TOTP_ISSUER`, `HOST`, `PORT`, `LOG_LEVEL`, `PHOENIX_EXTENSIONS` (JSON catalogue of `{id,title,path}` only), and optional `REDIS_URL`.

## Usage Examples

### Default single-pod SQLite (recommended for Phase 1)

```bash
helm install uptime-phoenix ./charts/uptime-phoenix
# Results in: 1 Deployment, 1 PVC (/data), embedded SPA, in-process EventBus
```

### MariaDB mode (opt-in)

```bash
helm install uptime-phoenix ./charts/uptime-phoenix \
  --set database.engine=mariadb \
  --set mariadb.enabled=true \
  --set mariadbExternal.host=mariadb.example.com
```

### Multi-pod scaling with in-release Valkey

A checked-in production overlay enables split API + sharded workers + Valkey.
It expects an **external** shared MariaDB (`mariadbExternal.*`); this chart
does not run a MariaDB server.

```bash
helm upgrade --install uptime-phoenix ./charts/uptime-phoenix \
  -n uptime-phoenix --create-namespace \
  -f charts/uptime-phoenix/values-production-split.yaml \
  --set ingress.host=uptime.example.com \
  --set config.publicUrl=https://uptime.example.com \
  --set mariadbExternal.host=mariadb.example.svc.cluster.local \
  --set mariadbExternal.password='<app-user-password>'
```

Minimal `--set` form of the same topology:

```bash
helm upgrade --install uptime-phoenix ./charts/uptime-phoenix \
  --set mode=split \
  --set database.engine=mariadb \
  --set mariadbExternal.host=mariadb.example.svc.cluster.local \
  --set valkey.enabled=true \
  --set worker.replicas=3 \
  --set worker.shards.enabled=true
```

This deploys authenticated, persistent Valkey from the project-owned official
subchart. For standard Helm installs, Phoenix generates and retains an ACL
password Secret, and both the Valkey pod and Phoenix reference that Secret
without putting the password in a Deployment manifest. Customize the subchart
under `valkey.*`, for example `valkey.dataStorage.requestedSize=5Gi`. Set
`valkey.replica.enabled=true` for a primary plus replicas; Phoenix continues to
use the primary Service. The default Valkey NetworkPolicy allows clients from
the release namespace.

For GitOps rendering, use a pre-created or externally managed Secret so a
renderer without cluster `lookup` access cannot generate a new password. The
Secret must contain the default user's password under key `default`:

```yaml
valkey:
  enabled: true
  auth:
    managedSecret: false
    usersExistingSecret: phoenix-valkey-auth
```

To use an external Redis server instead, keep `valkey.enabled=false` and enable
the external path:

```bash
helm upgrade --install uptime-phoenix ./charts/uptime-phoenix \
  --set redis.enabled=true \
  --set redis.host=redis.example.internal \
  --set redis.password='<password>'
```

For production credentials, prefer a pre-created Secret containing the full
URL so the password does not appear in Helm arguments or values:

```bash
kubectl create secret generic phoenix-redis \
  --from-literal=redis-url='rediss://default:<password>@redis.example.internal:6379/0'
helm upgrade --install uptime-phoenix ./charts/uptime-phoenix \
  --set redis.enabled=true \
  --set redis.existingSecret=phoenix-redis
```

`valkey.enabled` and `redis.enabled` are mutually exclusive.

### Ingress with custom host

The chart includes `nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"` for long-lived WebSocket connections.

### Expose Uptime Phoenix via Cloudflare Tunnel

Run a `cloudflared` connector that dials out to Cloudflare and tunnels traffic to
the in-cluster Uptime Phoenix Service — no public Ingress controller, LoadBalancer, or
inbound ports required. Cloudflare terminates TLS, so you normally disable the
chart's Ingress.

1. **Create a named tunnel** in the Cloudflare Zero Trust dashboard
   (*Networks → Tunnels → Create a tunnel → Cloudflared*), or via CLI:

   ```bash
   cloudflared tunnel login
   cloudflared tunnel create phoenix
   ```

2. **Add a Public Hostname route** pointing at the Uptime Phoenix Service, e.g.
   service `http://<release>-api:3000` (split mode) or `http://<release>:3000`
   (single-pod mode). The dashboard then shows the connector **token**.

3. **Set values and install/upgrade:**

   ```bash
   helm upgrade --install uptime-phoenix ./charts/uptime-phoenix \
     --set ingress.enabled=false \
     --set cloudflareTunnel.enabled=true \
     --set cloudflareTunnel.token='<your-connector-token>'
   ```

   Prefer a pre-created Secret? Skip `token` and reference it:

   ```bash
   kubectl create secret generic my-cf-tunnel --from-literal=token='<token>'
   helm upgrade --install uptime-phoenix ./charts/uptime-phoenix \
     --set ingress.enabled=false \
     --set cloudflareTunnel.enabled=true \
     --set cloudflareTunnel.existingSecret=my-cf-tunnel
   ```

The connector reads the token via the `TUNNEL_TOKEN` env var; no certificates or
config files are mounted. Set `cloudflareTunnel.replicas=2` (the default) for HA.

### Sidebar extensions (optional)

`extensions: []` by default — no extra Deployments and an empty `PHOENIX_EXTENSIONS`
catalogue. Each item is a separate Deployment + ClusterIP Service + Ingress
`Prefix` path (inserted after `/api` and `/ws`, before `/`).

Phoenix gates the sidebar entries and the `GET /api/extensions` catalog behind the
per-user **View extensions** capability (`Settings → Users → Access`; admins always
see extensions). Note the boundary: the plugin's direct Ingress path is served by
its own Deployment, not proxied through Phoenix, so a plugin that needs to stay
private must enforce its own authentication.

The plugin **image is operator-supplied**. This chart does not ship plugin
images, does not run `CREATE USER` / SQL, and does not mount Phoenix's `DB_DSN`.
Provision any dedicated database user and Secret yourself, then point
`database.secretName` at it (`DATABASE_DSN`).

Readiness defaults to `GET /health/ready` on the plugin port. Override with
`readinessPath` if the image serves a different probe.

The sidebar icon is **not** extracted from the container image (Helm cannot
do that). The plugin must serve a same-origin file. Default is
`GET {path}/icon.svg` (for `path: /storage` that is `/storage/icon.svg`).
Override with `icon` if the image uses another path (`/storage/favicon.ico`).
If the file is missing, the admin UI falls back to a generic plug icon.

```yaml
extensions:
  - id: ecs-usage
    title: Storage
    path: /storage
    image: ghcr.io/myteam/ecs-usage:1.2.3
    port: 80
    database:
      secretName: ecs-usage-dsn
      secretKey: dsn
```

Paths must not equal or nest under reserved Phoenix routes (`/api`, `/ws`,
`/dashboard`, `/insights`, `/monitors`, `/alerts`, `/notifications`,
`/escalation-policies`, `/maintenance`, `/status-pages`, `/incidents`,
`/backup`, `/settings`, `/login`, `/docs`, `/extensions`). Duplicate `id` or
`path` values fail the render.

Cloudflare Tunnel users must add each extension path (e.g. `/storage`) as a
Public Hostname route themselves — the chart only wires cluster Ingress.

## Verification

```bash
helm lint charts/uptime-phoenix
helm template charts/uptime-phoenix --set scaling.mode=single
helm template charts/uptime-phoenix --set scaling.mode=multi \
  --set redis.enabled=true --set redis.host=redis.example.internal
helm template charts/uptime-phoenix --set mode=api --set hpa.enabled=true
helm template charts/uptime-phoenix --set valkey.enabled=true
helm template uptime-phoenix charts/uptime-phoenix \
  --set extensions[0].id=ecs-usage \
  --set extensions[0].title=Storage \
  --set extensions[0].path=/storage \
  --set extensions[0].image=example.com/ecs-usage:1
```

All produce valid Kubernetes manifests. Reserved extension paths (`/api`,
`/dashboard`, …) fail `helm template` at render time.

## Notes

- Default values satisfy the **Minimal-Dependency Principle**: `helm install` works with no external DB/Redis.
- The optional dependency is the project-owned
  [official Valkey chart](https://github.com/valkey-io/valkey-helm), pinned in
  `Chart.lock` and vendored for reproducible/offline rendering.
- `readOnlyRootFilesystem: true` + `emptyDir` for `/tmp` for Go runtime temps.
- PDB `minAvailable: 1` on the Deployment.
- Health probes: `/api/health/live` and `/api/health/ready`.
- API, worker, and all-in-one pod templates include `checksum/config` and
  `checksum/secret` so an Argo CD sync / `helm upgrade` that changes a
  chart-managed ConfigMap or Secret rolls those pods. See
  [Helm & Argo CD](../../docs/guides/helm-and-argocd.md#configmap--secret-changes-restart-api-and-worker).

## License

MIT — see the [LICENSE](../../LICENSE) file at the repository root.
