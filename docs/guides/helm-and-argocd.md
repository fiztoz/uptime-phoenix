# Helm & Argo CD

Deploy **Uptime Phoenix** with the published Helm chart (`v0.2.0+`).

The chart is an **OCI package on GHCR**. Install it with an OCI URL:

```bash
helm upgrade --install uptime-phoenix \
  oci://ghcr.io/fiztoz/charts/uptime-phoenix \
  --version 0.2.0
```

| | |
|---|---|
| Chart | `uptime-phoenix` |
| Chart version | `0.2.0` |
| OCI chart | `oci://ghcr.io/fiztoz/charts/uptime-phoenix` |
| App image | `ghcr.io/fiztoz/uptime-phoenix:0.2.0` |
| Release assets | [v0.2.0](https://github.com/fiztoz/uptime-phoenix/releases/tag/v0.2.0) |

Also see: [binaries](binaries.md) · [Docker / GHCR](docker-ghcr.md) · [deployment modes](../DEPLOYMENT_MODES.md)

---

## Helm CLI

### Install (default: single pod, SQLite, embedded UI)

```bash
kubectl create namespace uptime-phoenix

helm upgrade --install uptime-phoenix \
  oci://ghcr.io/fiztoz/charts/uptime-phoenix \
  --version 0.2.0 \
  --namespace uptime-phoenix \
  --create-namespace \
  --set image.tag=0.2.0 \
  --set ingress.host=uptime.example.com
```

### Inspect

```bash
helm show chart oci://ghcr.io/fiztoz/charts/uptime-phoenix --version 0.2.0
helm show values oci://ghcr.io/fiztoz/charts/uptime-phoenix --version 0.2.0

helm template uptime-phoenix \
  oci://ghcr.io/fiztoz/charts/uptime-phoenix \
  --version 0.2.0 \
  --set image.tag=0.2.0
```

### Install with a values file

```bash
# values-uptime.yaml — your overrides only
cat > values-uptime.yaml <<'EOF'
image:
  repository: ghcr.io/fiztoz/uptime-phoenix
  tag: "0.2.0"

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
EOF

helm upgrade --install uptime-phoenix \
  oci://ghcr.io/fiztoz/charts/uptime-phoenix \
  --version 0.2.0 \
  --namespace uptime-phoenix --create-namespace \
  -f values-uptime.yaml
```

### Other install sources

```bash
# GitHub Release chart package
curl -fsSL -o uptime-phoenix-0.2.0.tgz \
  https://github.com/fiztoz/uptime-phoenix/releases/download/v0.2.0/uptime-phoenix-0.2.0.tgz
helm upgrade --install uptime-phoenix ./uptime-phoenix-0.2.0.tgz \
  -n uptime-phoenix --create-namespace --set image.tag=0.2.0

# From a git clone (chart source)
helm upgrade --install uptime-phoenix ./charts/uptime-phoenix \
  -n uptime-phoenix --create-namespace \
  --set image.repository=ghcr.io/fiztoz/uptime-phoenix \
  --set image.tag=0.2.0
```

### Upgrade / uninstall

```bash
helm upgrade uptime-phoenix \
  oci://ghcr.io/fiztoz/charts/uptime-phoenix \
  --version 0.2.0 \
  -n uptime-phoenix \
  -f values-uptime.yaml

helm uninstall uptime-phoenix -n uptime-phoenix
```

### Useful `--set` examples

```bash
# No ingress (port-forward / tunnel only)
--set ingress.enabled=false

# MariaDB instead of SQLite (still single app pod if mariadb.enabled=true)
--set database.engine=mariadb --set mariadb.enabled=true

# Truly split API + worker with Valkey managed by this Helm release
--set mode=split \
--set database.engine=mariadb \
--set mariadb.enabled=true \
--set valkey.enabled=true

# Truly split with an existing Redis server instead
--set mode=split \
--set database.engine=mariadb \
--set redis.enabled=true \
--set redis.host=redis.example.internal
```

`valkey.enabled=true` deploys the official Valkey chart with authentication and
persistence in the same release. Its ACL password is generated and retained in
a Secret. Customize it under `valkey.*`, such as
`valkey.dataStorage.requestedSize=5Gi` or `valkey.replica.enabled=true`.

For external Redis, `redis.existingSecret` can point to a Secret containing a
complete `redis://` or `rediss://` URL under `redis-url` (configurable with
`redis.existingSecretKey`). This is preferred over an inline password.

For Argo CD with in-release Valkey, create the password Secret through your
normal secret-management workflow and set:

```yaml
valkey:
  enabled: true
  auth:
    managedSecret: false
    usersExistingSecret: phoenix-valkey-auth
```

The Secret must contain the default ACL user's password under key `default`.
This avoids relying on Helm's cluster `lookup`, which is not available during
normal Argo CD manifest rendering.

Full mode matrix: [`docs/DEPLOYMENT_MODES.md`](../DEPLOYMENT_MODES.md).

---

## Argo CD

Point an Application at the OCI chart. Chart version has **no** leading `v`
(`0.2.0`, not `v0.2.0`).

### Basic Application (inline values)

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: uptime-phoenix
  namespace: argocd
  finalizers:
    - resources-finalizer.argocd.argoproj.io
spec:
  project: default
  source:
    repoURL: ghcr.io/fiztoz/charts
    chart: uptime-phoenix
    targetRevision: 0.2.0
    helm:
      releaseName: uptime-phoenix
      values: |
        image:
          repository: ghcr.io/fiztoz/uptime-phoenix
          tag: "0.2.0"
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
```

```bash
kubectl apply -f uptime-phoenix-application.yaml
```

### Override with your own values file (GitOps)

Keep chart from OCI; keep **overrides** in your config repo.

**Layout of your config repo:**

```text
my-gitops/
  uptime-phoenix/
    Application.yaml
    values.yaml          # your overrides
```

**`values.yaml` (overrides only):**

```yaml
image:
  repository: ghcr.io/fiztoz/uptime-phoenix
  tag: "0.2.0"
  pullPolicy: IfNotPresent

ingress:
  enabled: true
  className: nginx
  host: uptime.example.com
  tls: true
  tlsSecretName: uptime-phoenix-tls
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod

database:
  engine: sqlite
  persistence:
    enabled: true
    size: 5Gi

resources:
  requests:
    cpu: 100m
    memory: 256Mi
  limits:
    cpu: "1"
    memory: 512Mi
```

**`Application.yaml` with multi-source (chart OCI + values from git):**

Argo CD multi-source merges a Helm chart with value files from another repo:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: uptime-phoenix
  namespace: argocd
  finalizers:
    - resources-finalizer.argocd.argoproj.io
spec:
  project: default
  sources:
    # 1) Helm chart from GHCR
    - repoURL: ghcr.io/fiztoz/charts
      chart: uptime-phoenix
      targetRevision: 0.2.0
      helm:
        releaseName: uptime-phoenix
        valueFiles:
          - $values/uptime-phoenix/values.yaml
    # 2) Your override values (this git repo)
    - repoURL: https://github.com/YOUR_ORG/my-gitops.git
      targetRevision: main
      ref: values
  destination:
    server: https://kubernetes.default.svc
    namespace: uptime-phoenix
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

`$values` is the `ref` name of the second source. Path is relative to that repo root.

### Single-source git (fork / monorepo chart path)

When you track this repo’s chart directory and override with a file next to it:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: uptime-phoenix
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/fiztoz/uptime-phoenix.git
    targetRevision: v0.2.0
    path: charts/uptime-phoenix
    helm:
      # File paths are relative to path: (chart directory)
      # Put a custom file in your fork, or use valuesObject / values for overrides.
      valueFiles:
        - values.yaml
      # Extra overrides on top of valueFiles:
      values: |
        image:
          tag: "0.2.0"
        ingress:
          host: uptime.example.com
  destination:
    server: https://kubernetes.default.svc
    namespace: uptime-phoenix
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

### GitOps repo holding only overrides + Application (chart path in valuesObject)

If multi-source is unavailable, embed values (or use `helm.parameters`) and pin chart via OCI single source + `values` block as in the basic example.

Another pattern: store a full values file in git and reference it only when using the **git chart path** with a custom values file in a fork:

```text
charts/uptime-phoenix/
  values.yaml              # chart defaults (upstream)
  values-production.yaml   # your file (in a fork or overlay commit)
```

```yaml
source:
  repoURL: https://github.com/YOUR_ORG/uptime-phoenix.git
  targetRevision: main
  path: charts/uptime-phoenix
  helm:
    valueFiles:
      - values.yaml
      - values-production.yaml   # later files win for same keys
```

### Multiple value files (layering)

Helm/Argo merge order: later files override earlier ones.

```yaml
helm:
  valueFiles:
    - values-base.yaml
    - values-prod.yaml
  values: |
    # highest precedence for these keys
    ingress:
      host: uptime.example.com
```

### Split mode Application values example

Use the chart overlay `values-production-split.yaml` for production: split API,
sharded workers, and in-release Valkey. It requires an operator-managed MariaDB.

```bash
helm upgrade --install uptime-phoenix \
  oci://ghcr.io/fiztoz/charts/uptime-phoenix \
  --version 0.2.0 \
  -n uptime-phoenix --create-namespace \
  -f values-production-split.yaml \
  --set image.tag=0.2.0 \
  --set ingress.host=uptime.example.com \
  --set config.publicUrl=https://uptime.example.com \
  --set mariadbExternal.host=mariadb.example.svc.cluster.local \
  --set mariadbExternal.password='<app-user-password>'
```

From a git checkout the file lives at
`charts/uptime-phoenix/values-production-split.yaml`. After `helm pull --untar`
it is next to `values.yaml` in the unpacked chart.

```yaml
# values-split.yaml — smaller overlay if you do not want the production file
image:
  tag: "0.2.0"

mode: split

database:
  engine: mariadb
  persistence:
    enabled: false

mariadbExternal:
  host: mariadb.example.svc.cluster.local
  port: 3306
  database: phoenix
  username: phoenix
  password: ""   # prefer --set or a secrets overlay; do not commit

valkey:
  enabled: true

api:
  replicas: 3

worker:
  replicas: 3
  shards:
    enabled: true

ingress:
  enabled: true
  host: uptime.example.com
```

### Sync & check

```bash
argocd app get uptime-phoenix
argocd app sync uptime-phoenix
argocd app logs uptime-phoenix

kubectl -n uptime-phoenix get pods,svc,ingress,pvc
```

---

## After deploy

```bash
kubectl -n uptime-phoenix get pods
kubectl -n uptime-phoenix port-forward svc/uptime-phoenix 3000:3000
# open http://127.0.0.1:3000
curl -sS http://127.0.0.1:3000/api/health/live
```

Service name may include the release prefix (e.g. `uptime-phoenix`). Use
`kubectl -n uptime-phoenix get svc` if unsure.

---

## Related

| Guide | Topic |
|---|---|
| [binaries.md](binaries.md) | Release binaries + env for `all` / `api` / `worker` |
| [docker-ghcr.md](docker-ghcr.md) | Pull and run images from GHCR |
| [DEPLOYMENT_MODES.md](../DEPLOYMENT_MODES.md) | All-in-one vs split architecture |
| [charts/uptime-phoenix/README.md](../../charts/uptime-phoenix/README.md) | Chart values reference |
