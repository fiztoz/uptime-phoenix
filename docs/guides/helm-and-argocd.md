# Helm install & Argo CD guide

How to deploy **Uptime Phoenix** from the published Helm chart (v0.1.0+) with plain
Helm CLI or Argo CD.

> **Short answer:** you do **not** need `helm repo add`. The chart is published as a
> **Helm OCI artifact** on GHCR. Install with `helm install … oci://…`.

---

## What was published

| Item | Value |
|---|---|
| Chart name | `uptime-phoenix` |
| Chart version | `0.1.0` (matches git tag `v0.1.0`) |
| OCI location | `oci://ghcr.io/fiztoz/charts/uptime-phoenix` |
| Default image | `ghcr.io/fiztoz/uptime-phoenix:0.1.0` (pin the tag; do not rely on `latest` in prod) |
| GitHub Release assets | [v0.1.0](https://github.com/fiztoz/uptime-phoenix/releases/tag/v0.1.0) (`.tgz` + binaries) |

Release pipeline pushes the chart with:

```text
helm push uptime-phoenix-0.1.0.tgz oci://ghcr.io/fiztoz/charts
# → ghcr.io/fiztoz/charts/uptime-phoenix:0.1.0
```

There is **no** traditional `index.yaml` Helm repo. `helm repo add` / `helm search repo`
do not apply to this package.

---

## Prerequisites

- Helm **3.8+** (OCI support)
- Kubernetes cluster + `kubectl` context
- For private GHCR packages only: a GitHub token with `read:packages`

```bash
# Optional — only if the package is private or you hit 401/403
export CR_PAT=ghp_xxxxxxxx   # classic PAT: read:packages (and write:packages if you publish)
echo "$CR_PAT" | helm registry login ghcr.io -u YOUR_GITHUB_USERNAME --password-stdin
```

If the package is public (common when the GitHub repo is public), you can skip login.

---

## Install with Helm CLI

### 1) OCI install (recommended)

```bash
# Create a namespace
kubectl create namespace uptime-phoenix

# Install the chart from GHCR (no helm repo add)
helm upgrade --install uptime-phoenix \
  oci://ghcr.io/fiztoz/charts/uptime-phoenix \
  --version 0.1.0 \
  --namespace uptime-phoenix \
  --create-namespace \
  --set image.tag=0.1.0 \
  --set ingress.host=uptime.example.com
```

**Default shape:** single pod, SQLite on a PVC, embedded UI, in-process EventBus —
zero external dependencies.

### 2) Inspect before install

```bash
# Show chart metadata
helm show chart oci://ghcr.io/fiztoz/charts/uptime-phoenix --version 0.1.0

# Show default values
helm show values oci://ghcr.io/fiztoz/charts/uptime-phoenix --version 0.1.0

# Render manifests without applying
helm template uptime-phoenix \
  oci://ghcr.io/fiztoz/charts/uptime-phoenix \
  --version 0.1.0 \
  --set image.tag=0.1.0 \
  --set ingress.enabled=false
```

### 3) Install from the GitHub Release `.tgz` (offline / air-gap)

```bash
# Download from the release page, or:
curl -fsSL -o uptime-phoenix-0.1.0.tgz \
  https://github.com/fiztoz/uptime-phoenix/releases/download/v0.1.0/uptime-phoenix-0.1.0.tgz

helm upgrade --install uptime-phoenix ./uptime-phoenix-0.1.0.tgz \
  --namespace uptime-phoenix --create-namespace \
  --set image.tag=0.1.0
```

### 4) Install from a git checkout (dev)

```bash
git clone https://github.com/fiztoz/uptime-phoenix.git
cd uptime-phoenix
helm upgrade --install uptime-phoenix ./charts/uptime-phoenix \
  --namespace uptime-phoenix --create-namespace \
  --set image.repository=ghcr.io/fiztoz/uptime-phoenix \
  --set image.tag=0.1.0
```

### 5) Common upgrades

```bash
# Bump app image to a new release (example)
helm upgrade uptime-phoenix \
  oci://ghcr.io/fiztoz/charts/uptime-phoenix \
  --version 0.1.0 \
  --namespace uptime-phoenix \
  --reuse-values \
  --set image.tag=0.1.0

# Uninstall
helm uninstall uptime-phoenix -n uptime-phoenix
# PVCs are often retained — delete manually if you want a clean slate:
# kubectl delete pvc -n uptime-phoenix -l app.kubernetes.io/instance=uptime-phoenix
```

---

## Recommended values (production-ish)

Save as `values-prod.yaml` (or embed under Argo CD `helm.values`):

```yaml
# Pin the released image — do not leave tag: latest in production
image:
  repository: ghcr.io/fiztoz/uptime-phoenix
  tag: "0.1.0"
  pullPolicy: IfNotPresent

# Single-pod all-in-one is fine to start; switch to split later (see DEPLOYMENT_MODES.md)
mode: all

database:
  engine: sqlite          # or mariadb for multi-replica / split
  persistence:
    enabled: true
    size: 5Gi
    # storageClass: ""    # set if your cluster has no default StorageClass

ingress:
  enabled: true
  className: nginx
  host: uptime.example.com
  tls: true
  tlsSecretName: uptime-phoenix-tls
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    # WebSocket-friendly timeouts (already defaults in the chart)
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"

resources:
  requests:
    cpu: 100m
    memory: 256Mi
  limits:
    cpu: "1"
    memory: 512Mi
```

Install:

```bash
helm upgrade --install uptime-phoenix \
  oci://ghcr.io/fiztoz/charts/uptime-phoenix \
  --version 0.1.0 \
  --namespace uptime-phoenix --create-namespace \
  -f values-prod.yaml
```

### First login

On first boot, bootstrap the admin user via env (if your chart templates support
bootstrap vars) or use the UI/API self-bootstrap path for the first user only.
After the first user exists, open registration is disabled — further users are
admin-created. See `docs/RUNBOOK.md` and `Agents.md` auth notes.

### Private GHCR pull

If the container image is private, create a pull secret and reference it:

```bash
kubectl create secret docker-registry ghcr-pull \
  --namespace uptime-phoenix \
  --docker-server=ghcr.io \
  --docker-username=YOUR_GITHUB_USERNAME \
  --docker-password="$CR_PAT"

# values snippet:
# imagePullSecrets:
#   - name: ghcr-pull
```

---

## Deployment shapes (quick map)

| Goal | Key values |
|---|---|
| Zero deps, single pod | defaults (`mode=all`, `database.engine=sqlite`) |
| MariaDB in-cluster (still one app pod) | `database.engine=mariadb`, `mariadb.enabled=true` |
| API + worker split | `mode=split`, shared MariaDB, `redis.enabled=true` |
| No Ingress (Cloudflare Tunnel) | `ingress.enabled=false`, `cloudflareTunnel.enabled=true`, tunnel token |

Full mode matrix: [`docs/DEPLOYMENT_MODES.md`](../DEPLOYMENT_MODES.md).

---

## Argo CD Application (Helm OCI)

Argo CD **does not** use `helm repo add` either. Point the Application at the OCI
registry path and chart version.

### Minimal Application (public OCI chart)

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: uptime-phoenix
  namespace: argocd          # Argo CD control-plane namespace
  finalizers:
    - resources-finalizer.argocd.argoproj.io
spec:
  project: default

  source:
    # OCI Helm repository (no oci:// scheme on older Argo CD; see note below)
    repoURL: ghcr.io/fiztoz/charts
    chart: uptime-phoenix
    targetRevision: 0.1.0     # chart version, not the git tag form
    helm:
      releaseName: uptime-phoenix
      values: |
        image:
          repository: ghcr.io/fiztoz/uptime-phoenix
          tag: "0.1.0"
        ingress:
          enabled: true
          className: nginx
          host: uptime.example.com
          tls: true
          tlsSecretName: uptime-phoenix-tls
        database:
          engine: sqlite
          persistence:
            enabled: true
            size: 5Gi

  destination:
    server: https://kubernetes.default.svc
    namespace: uptime-phoenix

  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
      - ServerSideApply=true
    retry:
      limit: 5
      backoff:
        duration: 5s
        factor: 2
        maxDuration: 3m
```

Apply:

```bash
kubectl apply -f uptime-phoenix-application.yaml
# or: argocd app create -f uptime-phoenix-application.yaml
```

### Alternative: `oci://` in `repoURL`

Some Argo CD versions accept:

```yaml
source:
  repoURL: oci://ghcr.io/fiztoz/charts
  chart: uptime-phoenix
  targetRevision: 0.1.0
```

If sync fails with “unsupported protocol” or “repository not found”, use the form
**without** the `oci://` prefix (`repoURL: ghcr.io/fiztoz/charts`) — that is the
most common working shape on Argo CD 2.6+.

### Private GHCR (chart and/or image)

1. **Repository credential for the chart** (Argo CD UI → Settings → Repositories,
   or a Secret):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: ghcr-helm-oci
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
stringData:
  # For Helm OCI repositories
  type: helm
  name: ghcr-fiztoz-charts
  url: ghcr.io/fiztoz/charts
  enableOCI: "true"
  username: YOUR_GITHUB_USERNAME
  password: ghp_xxxxxxxx   # PAT with read:packages
```

2. **Image pull secret** in the *destination* namespace (see earlier
   `docker-registry` secret), plus:

```yaml
# under helm.values
imagePullSecrets:
  - name: ghcr-pull
```

### Prefer ExternalSecrets / sealed values for secrets

Do **not** put tunnel tokens, DB passwords, or PATs in plain `helm.values` in git.
Use:

- Argo CD **External Secrets** / Sealed Secrets / SOPS, or
- `helm.valueFiles` pointing at an encrypted file, or
- chart keys that reference an existing Secret (`cloudflareTunnel.existingSecret`,
  external MariaDB password via Secret injection if you extend the chart).

### Git-directory source (track chart from this monorepo)

If you fork the repo and want Argo CD to render from git instead of OCI:

```yaml
spec:
  source:
    repoURL: https://github.com/fiztoz/uptime-phoenix.git
    targetRevision: v0.1.0          # git tag / branch / SHA
    path: charts/uptime-phoenix
    helm:
      valueFiles:
        - values.yaml               # chart defaults
      # Or valuesObject / values for overrides
      values: |
        image:
          tag: "0.1.0"
  destination:
    server: https://kubernetes.default.svc
    namespace: uptime-phoenix
```

This is useful for chart development; production consumers usually prefer the
**OCI chart + pinned image tag**.

### ApplicationSet (optional multi-cluster)

```yaml
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: uptime-phoenix
  namespace: argocd
spec:
  generators:
    - list:
        elements:
          - cluster: in-cluster
            url: https://kubernetes.default.svc
            host: uptime.example.com
  template:
    metadata:
      name: 'uptime-phoenix-{{cluster}}'
    spec:
      project: default
      source:
        repoURL: ghcr.io/fiztoz/charts
        chart: uptime-phoenix
        targetRevision: 0.1.0
        helm:
          values: |
            image:
              tag: "0.1.0"
            ingress:
              host: {{host}}
      destination:
        server: '{{url}}'
        namespace: uptime-phoenix
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
        syncOptions:
          - CreateNamespace=true
```

---

## Verify after deploy

```bash
kubectl -n uptime-phoenix get pods,svc,ingress,pvc
kubectl -n uptime-phoenix logs deploy/uptime-phoenix -f   # name may include release prefix

# Health
kubectl -n uptime-phoenix port-forward svc/uptime-phoenix 3000:3000
curl -sS http://127.0.0.1:3000/api/health/live
```

Argo CD:

```bash
argocd app get uptime-phoenix
argocd app sync uptime-phoenix
argocd app logs uptime-phoenix
```

---

## FAQ

### Do I need `helm repo add`?

**No.** This chart is **OCI-only** on GHCR:

```bash
# ✅ correct
helm install uptime-phoenix oci://ghcr.io/fiztoz/charts/uptime-phoenix --version 0.1.0

# ❌ not applicable
helm repo add uptime-phoenix https://...
helm install uptime-phoenix uptime-phoenix/uptime-phoenix
```

### Chart version vs image tag

| Field | Meaning |
|---|---|
| `--version 0.1.0` / Argo `targetRevision` | **Helm chart** package version |
| `image.tag: "0.1.0"` | **Container image** tag on GHCR |

Keep them in lockstep for releases unless you intentionally mix chart and image.

### Chart version vs git tag

| Git tag | Chart version | Image tag |
|---|---|---|
| `v0.1.0` | `0.1.0` | `0.1.0` |

Drop the leading `v` for Helm/OCI versions.

### Upgrade path

1. Publish a new release (e.g. `v0.1.1`) → new chart + images on GHCR.
2. Bump Argo `targetRevision` and `image.tag`, or run:

```bash
helm upgrade uptime-phoenix oci://ghcr.io/fiztoz/charts/uptime-phoenix \
  --version 0.1.1 -n uptime-phoenix \
  --set image.tag=0.1.1
```

---

## Related docs

- Chart defaults & knobs: [`charts/uptime-phoenix/README.md`](../../charts/uptime-phoenix/README.md)
- Deploy modes (all / split / api / worker): [`docs/DEPLOYMENT_MODES.md`](../DEPLOYMENT_MODES.md)
- Ops runbook: [`docs/RUNBOOK.md`](../RUNBOOK.md)
- Release / publish pipeline: [`docs/RELEASING.md`](../RELEASING.md)
