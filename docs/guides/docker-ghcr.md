# Docker images from GHCR

Pull and run **Uptime Phoenix** container images published to GitHub Container Registry.

Images are **public** with the open-source repo (no login required for pull).

Also see: [Helm & Argo CD](helm-and-argocd.md) · [binaries](binaries.md) · [DEPLOYMENT_MODES.md](../DEPLOYMENT_MODES.md)

---

## Images (v0.2.2)

| Image | Use |
|---|---|
| `ghcr.io/fiztoz/uptime-phoenix:0.2.2` | All-in-one (API + worker + embedded UI) |
| `ghcr.io/fiztoz/uptime-phoenix:latest` | Same, floating tag (prefer pin for production) |
| `ghcr.io/fiztoz/uptime-phoenix-api:0.2.2` | API tier only |
| `ghcr.io/fiztoz/uptime-phoenix-worker:0.2.2` | Worker tier only |
| `ghcr.io/fiztoz/uptime-phoenix-web:0.2.2` | Static UI (nginx) for split web tier |

Multi-arch: `linux/amd64`, `linux/arm64`. Images are **keyless cosign**-signed (optional verify below).

---

## Pull

```bash
docker pull ghcr.io/fiztoz/uptime-phoenix:0.2.2

# Split images
docker pull ghcr.io/fiztoz/uptime-phoenix-api:0.2.2
docker pull ghcr.io/fiztoz/uptime-phoenix-worker:0.2.2
docker pull ghcr.io/fiztoz/uptime-phoenix-web:0.2.2
```

No `docker login` needed for public packages. If a pull returns 401/403, ensure the
package visibility is public on GHCR or run `docker login ghcr.io`.

---

## All-in-one (quick start)

Single container, SQLite on a volume, bootstrap admin on first start:

```bash
docker run -d --name uptime-phoenix \
  -p 3000:3000 \
  -e JWT_SECRET="$(openssl rand -hex 32)" \
  -e BOOTSTRAP_USERNAME=admin \
  -e BOOTSTRAP_PASSWORD='ChangeMe123!' \
  -e DB_ENGINE=sqlite \
  -e DB_DSN='file:/data/phoenix.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)' \
  -e HOST=0.0.0.0 \
  -e PORT=3000 \
  -v uptime-phoenix-data:/data \
  ghcr.io/fiztoz/uptime-phoenix:0.2.2

# open http://localhost:3000
docker logs -f uptime-phoenix
```

Compose equivalent:

```yaml
# compose.yaml
services:
  uptime-phoenix:
    image: ghcr.io/fiztoz/uptime-phoenix:0.2.2
    ports:
      - "3000:3000"
    environment:
      JWT_SECRET: ${JWT_SECRET:-change_me_jwt}
      BOOTSTRAP_USERNAME: ${BOOTSTRAP_USERNAME:-admin}
      BOOTSTRAP_PASSWORD: ${BOOTSTRAP_PASSWORD:-ChangeMe123!}
      DB_ENGINE: sqlite
      DB_DSN: file:/data/phoenix.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)
      HOST: "0.0.0.0"
      PORT: "3000"
      LOG_LEVEL: info
    volumes:
      - uptime-phoenix-data:/data
    restart: unless-stopped

volumes:
  uptime-phoenix-data:
```

```bash
docker compose up -d
```

---

## All-in-one + external MariaDB

```bash
docker run -d --name uptime-phoenix \
  -p 3000:3000 \
  -e JWT_SECRET=… \
  -e BOOTSTRAP_USERNAME=admin \
  -e BOOTSTRAP_PASSWORD='ChangeMe123!' \
  -e DB_ENGINE=mariadb \
  -e DB_DSN='phoenix:SECRET@tcp(host.docker.internal:3306)/phoenix?parseTime=true&loc=UTC&multiStatements=true' \
  ghcr.io/fiztoz/uptime-phoenix:0.2.2
```

Use a real hostname reachable from the container instead of `host.docker.internal` in production.

---

## Split stack (api + worker + redis + mariadb)

Minimal sketch (adapt passwords and networks for your environment):

```yaml
# compose.split.ghcr.yaml
services:
  mariadb:
    image: mariadb:11
    environment:
      MARIADB_ROOT_PASSWORD: rootsecret
      MARIADB_DATABASE: phoenix
      MARIADB_USER: phoenix
      MARIADB_PASSWORD: appsecret
    volumes:
      - mariadb_data:/var/lib/mysql
    healthcheck:
      test: ["CMD", "healthcheck.sh", "--connect", "--innodb_initialized"]
      interval: 10s
      timeout: 5s
      retries: 10

  redis:
    image: redis:7-alpine
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      retries: 5

  api:
    image: ghcr.io/fiztoz/uptime-phoenix-api:0.2.2
    depends_on:
      mariadb:
        condition: service_healthy
      redis:
        condition: service_healthy
    environment:
      JWT_SECRET: change_me_jwt
      BOOTSTRAP_USERNAME: admin
      BOOTSTRAP_PASSWORD: ChangeMe123!
      DB_ENGINE: mariadb
      DB_DSN: phoenix:appsecret@tcp(mariadb:3306)/phoenix?parseTime=true&loc=UTC&multiStatements=true
      REDIS_URL: redis://redis:6379/0
      HOST: "0.0.0.0"
      PORT: "3000"
    ports:
      - "3000:3000"
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://127.0.0.1:3000/api/health/ready"]
      interval: 10s
      retries: 12
      start_period: 30s

  worker:
    image: ghcr.io/fiztoz/uptime-phoenix-worker:0.2.2
    depends_on:
      api:
        condition: service_healthy
    environment:
      JWT_SECRET: change_me_jwt
      DB_ENGINE: mariadb
      DB_DSN: phoenix:appsecret@tcp(mariadb:3306)/phoenix?parseTime=true&loc=UTC&multiStatements=true
      REDIS_URL: redis://redis:6379/0
      LOG_LEVEL: info

volumes:
  mariadb_data:
```

```bash
docker compose -f compose.split.ghcr.yaml up -d
# UI + API: http://localhost:3000
```

Optional separate web image (nginx SPA) is `ghcr.io/fiztoz/uptime-phoenix-web:0.2.2` —
proxy `/api` and `/ws` to the API service (see `docker-compose.split.yml` in the repo
for a full local reference).

Env reference for each role: [binaries.md](binaries.md).

---

## Verify image signature (optional)

Releases are signed with **keyless cosign** (Sigstore).

```bash
# Install cosign, then:
cosign verify \
  --certificate-identity-regexp='https://github.com/fiztoz/uptime-phoenix/.*' \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com \
  ghcr.io/fiztoz/uptime-phoenix:0.2.2
```

Exact identity patterns are described in [`docs/RELEASING.md`](../RELEASING.md).

---

## Tags & upgrades

```bash
# Pin in production
docker pull ghcr.io/fiztoz/uptime-phoenix:0.2.2

# Move to a newer release
docker pull ghcr.io/fiztoz/uptime-phoenix:0.2.3
docker stop uptime-phoenix && docker rm uptime-phoenix
# re-run with the new tag (keep the same volume for SQLite)
```

Prefer **version tags** (`0.2.2`) over `latest` for reproducible deploys.

---

## Related compose files in this repo

| File | Purpose |
|---|---|
| `docker-compose.yml` | Local build + MariaDB (dev) |
| `docker-compose.split.yml` | Local split build (api/worker/web + MariaDB + Redis) |

Those **build from source**. This guide uses **prebuilt GHCR images** for operators
who do not want to compile.
