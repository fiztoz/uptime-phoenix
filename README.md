# Uptime Phoenix

> **Status: hobby project — not under active development.**
>
> This repository is shared as-is for learning, reference, and forking. There is **no
> support SLA**, no guaranteed security response time, and no commitment to merge PRs or
> ship releases. If you want to run or extend it seriously, **fork it** and maintain your
> own copy. See [SECURITY.md](SECURITY.md) for how (and whether) to report issues.

Uptime Phoenix is a self-hosted uptime monitoring platform in the Uptime Kuma class, built as a
single static Go binary with an embedded Svelte 5 frontend. It follows a strict hexagonal
architecture (domain core, ports, adapters), runs on SQLite for zero-dependency setups or
MariaDB for production, and is Kubernetes-native via a Helm chart that scales from one
zero-dependency pod to a split API + worker deployment with Redis fan-out.

## Features

| Area | What you get |
|---|---|
| 13 monitor types | http, tcp, ping, dns, websocket, push (HMAC ingest), docker, mqtt, rabbitmq, grpc, snmp, database (postgres/mysql/mariadb/mongodb/redis/mssql), s3 (AWS/MinIO/S3-compatible, health only) |
| 11 notification providers | bark, discord, feishu, gotify, line, mattermost, slack, smtp, teams, telegram, webhook — with shared retry/backoff |
| Alerting pipeline | Retry-confirm (PENDING to DOWN after max retries), resend interval, recovery notices, maintenance suppression, ack/resolve, escalation policies |
| Folder alerting | Monitor groups (folders) alert on their own rollup transition, not per child |
| Status pages | Slug + custom domain resolution, password gate, custom CSS, three dashboard styles (full / pills / grid), three-level public health, response-time charts, 90-day uptime, incidents with severity |
| Badges | Embeddable SVG status badges (viewBox-scalable) |
| RBAC | Per-monitor/group grants, capability flags (view extensions, create monitors/groups, manage notifications/maintenance), admin grant UI, RBAC-scoped WebSocket hub |
| Auth | JWT sessions, TOTP 2FA, WebAuthn passkeys, opt-in OIDC SSO, scoped API keys (hashed at rest); open registration disabled by design — first admin via bootstrap env vars |
| Config-as-code | Versioned `phoenix.dev/v1` YAML (`cmd/phoenix-config` + admin API): validate / plan / apply with secret redaction |
| Maintenance windows | Single and cron strategies with real alert suppression (cron evaluates UTC) |
| Backup | Full config export/import with ID remapping |
| Proxies | Outbound HTTP/HTTPS/SOCKS5, assignable per monitor |
| Observability | Prometheus `/metrics` (API-key protected), health endpoints, heartbeat retention + 1m/1h/1d rollups |
| i18n | English and Thai (`web/messages/en.json`, `web/messages/th.json`) |
| Deployment | Single binary, Docker Compose (all-in-one or truly split), Helm chart with `all` / `split` / `api` / `worker` modes |

## Quickstart

Requires Docker with the Compose plugin:

```bash
docker compose up
```

The first run builds the image, starts MariaDB, and boots Uptime Phoenix. Then open
**http://localhost:3000** and log in with the default bootstrap credentials:

| Field | Value |
|---|---|
| Username | `admin` |
| Password | `ChangeMe123!` |

Override the defaults (`MARIADB_PASSWORD`, `JWT_SECRET`, `BOOTSTRAP_USERNAME`,
`BOOTSTRAP_PASSWORD`, ...) via a `.env` file next to `docker-compose.yml`.

> **Do not expose these defaults to the internet.** Compose ships with well-known
> bootstrap credentials and a weak JWT secret for local convenience. For any shared or
> public deployment, set strong unique values for `JWT_SECRET`, `BOOTSTRAP_PASSWORD`,
> and database passwords, set `PRODUCTION=true`, and do not publish MariaDB (`3306`)
> beyond localhost.

Without Docker (Go 1.25+ and Bun required — SQLite, embedded UI, zero external
dependencies):

```bash
make run
# open http://localhost:3000 — same login as above
```

## Development

Prerequisites: Go 1.25+ (per `go.mod`) and Bun 1.0+. Bun is the only supported frontend
toolchain — never npm.

```bash
make dev-split
```

`dev-split` is the standard dev loop: it starts MariaDB and Redis in Docker, runs the API
locally on **:3000** with hot reload (air), the Vite dev server on **http://localhost:5173**
(proxies `/api` and `/ws` to the API), and the Docker worker once the API reports ready.
Login on a fresh DB: `admin / ChangeMe123!`.

Other loops: `make dev` (all-in-one, SQLite, hot reload) and `make help` for everything else.

Before finishing any change, run the gate:

```bash
make gate        # go build + vet + gofmt + race tests + web check/test/build
make gate-fast   # same, without the race detector
make gate-full   # the complete pre-merge gate: adds golangci-lint, frontend lint + e2e,
                  # helm lint/template, govulncheck, and git diff --check
```

**CI is restored** (owner, 2026-07-28): `.github/workflows/ci.yml` runs the gate on every
PR and push to `main` (backend, frontend, e2e, MariaDB contract, Helm, Docker). Local
`make gate-full` remains required for thoroughness and works offline — do not rely on CI
alone. See `docs/TESTING.md` for the full checklist and `docs/RELEASING.md` for release
dry-run / publish (`.github/workflows/release.yml`).

## Deployment

All modes are documented in detail in `docs/DEPLOYMENT_MODES.md`.

**Docker Compose, all-in-one** (MariaDB + Uptime Phoenix on :3000):

```bash
docker compose up
```

**Docker Compose, truly split** (MariaDB + Redis + API + worker + nginx web tier):

```bash
docker compose -f docker-compose.split.yml up --build
# open http://localhost:8080   (login: admin / ChangeMe123!)
```

**Helm, single pod** (zero external dependencies — SQLite on a PVC, embedded UI,
in-process event bus):

```bash
helm install uptime-phoenix ./charts/uptime-phoenix
```

**Helm, split mode** (API + worker in one release; requires shared MariaDB + Redis):

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

Key runtime configuration (source of truth: `internal/bootstrap/config.go`): `DB_ENGINE`
(`sqlite`|`mariadb`), `DB_DSN`, `JWT_SECRET`, `BOOTSTRAP_USERNAME`/`BOOTSTRAP_PASSWORD`,
`PORT` (default 3000), `MODE` (`all`|`api`|`worker`), `REDIS_URL` (opt-in event bus for
split mode), `HEARTBEAT_RETENTION_DAYS` (default 180, 0 disables), `LOG_LEVEL`.

## Screenshots

<!-- TODO: add screenshots — the premium dark UI is a selling point. Capture at least:
     dashboard, monitor detail, a public status page (grid style), and the admin
     settings/RBAC grant UI. Do not commit placeholders; add real captures. -->

Screenshots coming soon.

## Documentation

| Document | What it covers |
|---|---|
| [AGENTS.md](AGENTS.md) | Canonical rules: architecture boundaries, stack lock, plugin conventions, gate |
| [CONTRIBUTING.md](CONTRIBUTING.md) | How to contribute (humans and AI agents) |
| [SECURITY.md](SECURITY.md) | Vulnerability reporting and known risk model |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Technical design (hexagonal layout, source of truth) |
| [docs/TESTING.md](docs/TESTING.md) | Test commands, manual checklists, regression areas |
| [docs/LOCAL_DEVELOPMENT.md](docs/LOCAL_DEVELOPMENT.md) | Local dev setup, env vars, troubleshooting |
| [docs/DEPLOYMENT_MODES.md](docs/DEPLOYMENT_MODES.md) | Compose / Helm modes, Cloudflare Tunnel |
| [docs/RUNBOOK.md](docs/RUNBOOK.md) | Operator runbook |
| [docs/RELEASING.md](docs/RELEASING.md) | Release dry-run and optional publish (local + GitHub Actions) |
| [docs/ROADMAP.md](docs/ROADMAP.md) | Phase plans and completion status |
| [docs/PLAN.md](docs/PLAN.md) | Goal, scope, and locked decisions (kept current with the shipped tree) |
| [docs/LOADTEST.md](docs/LOADTEST.md) | Load-test results (10k monitors; Colima limits noted) |
| [docs/DESIGN.md](docs/DESIGN.md) | Design system for the UI |

## Project status & disclaimer

This is **hobby / vibe-coded software**, not a product with a vendor behind it.

- **Not actively maintained.** Issues and PRs may go unanswered. Prefer forking.
- **No warranty.** MIT license applies: use at your own risk.
- **No production guarantee.** Defaults favour local development convenience over hard
  fail-closed production posture (weak bootstrap password and JWT placeholders unless you
  override them).
- **Self-hosted trust model.** An authenticated operator can create monitors and
  notifications that reach arbitrary hosts and webhooks — that is intentional for an
  uptime tool, and it is also an SSRF-class capability if an admin account is compromised.
- **CI + local gate.** PRs run GitHub Actions (`ci.yml`); `make gate-full` is still the
  offline quality bar. Treat unreviewed contributions with caution.

If you adopt Uptime Phoenix in a real environment, you own hardening, upgrades, and incident
response.

## License

Uptime Phoenix is licensed under the [MIT License](LICENSE).

