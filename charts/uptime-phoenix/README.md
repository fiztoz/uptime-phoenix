# Uptime Phoenix Helm Chart

A self-hosted, K8s-native, minimal-dependency monitoring tool. The default `helm install` produces a single-pod deployment with SQLite, embedded frontend, and zero external services.

> **Published installs & Argo CD:** the chart is pushed to GHCR as an **OCI** package
> (`oci://ghcr.io/fiztoz/charts/uptime-phoenix`). You do **not** need `helm repo add`.
> Full guide (Helm CLI + Argo CD Application):
> [`docs/guides/helm-and-argocd.md`](../../docs/guides/helm-and-argocd.md).

## Quick Start

### From GHCR (recommended)

```bash
# No helm repo add — OCI install
helm upgrade --install uptime-phoenix \
  oci://ghcr.io/fiztoz/charts/uptime-phoenix \
  --version 0.1.0 \
  --namespace uptime-phoenix --create-namespace \
  --set image.tag=0.1.0
```

### From a git checkout

```bash
# Default (zero external dependencies, single pod, SQLite)
helm install uptime-phoenix ./charts/uptime-phoenix

# With custom values
helm install uptime-phoenix ./charts/uptime-phoenix -f my-values.yaml

# Upgrade
helm upgrade uptime-phoenix ./charts/uptime-phoenix
```

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `database.engine` | string | `sqlite` | `sqlite` (default, zero-dep) or `mariadb` |
| `database.persistence.enabled` | bool | `true` | Enable PVC for `/data` (SQLite DB file) |
| `database.persistence.size` | string | `1Gi` | PVC size for data |
| `mariadb.enabled` | bool | `false` | Enable MariaDB mode (requires external or additional setup) |
| `scaling.mode` | string | `single` | `single` \| `multi` \| `sharded` |
| `redis.enabled` | bool | `false` | Enable Redis (only for multi mode) |
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
| `config.totpIssuer` | string | `Phoenix` | TOTP issuer name |

Env vars exposed to container: `DB_ENGINE`, `DB_DSN`, `JWT_SECRET` (auto-generated in Secret), `JWT_EXPIRE_HOURS`, `TOTP_ISSUER`, `HOST`, `PORT`, `LOG_LEVEL`.

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

### Multi-pod scaling (Phase 2)

```bash
helm upgrade uptime-phoenix ./charts/uptime-phoenix \
  --set scaling.mode=multi \
  --set redis.enabled=true \
  --set web.split=true
```

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

## Verification

```bash
helm lint charts/uptime-phoenix
helm template charts/uptime-phoenix --set scaling.mode=single
helm template charts/uptime-phoenix --set scaling.mode=multi --set redis.enabled=true
```

All produce valid Kubernetes manifests.

## Notes

- Default values satisfy the **Minimal-Dependency Principle**: `helm install` works with no external DB/Redis.
- `readOnlyRootFilesystem: true` + `emptyDir` for `/tmp` for Go runtime temps.
- PDB `minAvailable: 1` on the Deployment.
- Health probes: `/api/health/live` and `/api/health/ready`.

## License

MIT — see the [LICENSE](../../LICENSE) file at the repository root.
