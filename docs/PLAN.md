# Uptime Phoenix — Project Plan

> A self-hosted, K8s-native monitoring tool. Uptime-Kuma-equivalent features, Port-and-Adapter architecture, Go + Svelte 5 stack.
>
> **Currency:** aligned with the shipped codebase as of 2026-07-28 (Phases 0–5 focused scope complete). Historical “early phase / deferred” language that no longer matches the tree has been corrected. Delivery detail lives in `docs/ROADMAP.md`; technical design in `docs/ARCHITECTURE.md`.

---

## 1. Goal

Build a self-hosted monitoring tool that matches Uptime Kuma's feature surface (uptime checks, notifications, public status pages, real-time dashboard) while being:

- **Minimal-dependency by default** — one pod, one PVC, zero external services required to function. `helm install` → one pod → works. External MariaDB, Redis, and the separate web tier are **opt-in**, never required.
- **K8s-native from day 1** — ships as a Helm chart; horizontal scaling is a config change, not a rewrite
- **Single-tenant and self-hosted** — each installation is owned and operated by the team deploying it; Uptime Phoenix is not a shared SaaS control plane
- **Platform-owned edge TLS** — Uptime Phoenix runs behind the cluster's load balancer/Ingress; the platform terminates and renews certificates
- **Frontend/backend separable** — same binary serves embedded static assets by default; split into independent Deployments when you need scale
- **Port-and-Adapter (hexagonal) architecture** — domain logic independent of frameworks, databases, and transports
- **Lean** — no required heavy infrastructure dependencies (no Kafka, message brokers, or external DB operators needed for Uptime Phoenix itself)

## 2. Scope

### Shipped monitor types (12)

- HTTP(s) — status code / keyword / JSON query assertion + TLS cert info
- TCP port connect
- ICMP Ping (unprivileged)
- DNS record query + assertion
- WebSocket upgrade probe
- Push (inbound HTTP receiver with HMAC)
- Docker container status (local socket or remote Docker API)
- MQTT broker connect + subscribe
- RabbitMQ AMQP 0-9-1 connect + optional queue/exchange passive declare
- gRPC health check
- SNMP GET
- Database connect health (PostgreSQL, MySQL, MariaDB, MongoDB, Redis, **MSSQL**) — fixed presets only (`ping` / `select_1`); never free-form operator SQL

### Shipped notification providers (11)

- Telegram, Discord, Slack, Email/SMTP, Generic Webhook, Microsoft Teams, Mattermost, Gotify, Bark, Feishu, Line

Do **not** add providers without user approval. Explicitly **not** shipped (backlog only if demand appears): ntfy, Pushover, PagerDuty, OpsGenie, Signal, Matrix, Pushbullet, Alerta, Zabbix, N8N.

### Core features (shipped)

- Real-time dashboard (single persistent WebSocket, Svelte 5 runes)
- Public status pages (live SPA, custom domain mapping, white-label, Atom/iCal feeds, uptime history)
- Tags + nested monitor groups (folders) with folder-level alerting
- Maintenance windows (single + cron strategies, IANA timezone)
- Auth: JWT sessions, TOTP 2FA, **WebAuthn/passkeys**, scoped API keys; bootstrap-only first user; admin user API + scoped RBAC
- Opt-in **OIDC SSO** with local break-glass
- **Config-as-code** (`phoenix.dev/v1` YAML validate/plan/apply) + JSON backup/restore
- Alert lifecycle (ack/resolve), escalation policies
- Prometheus `/metrics` (API-key protected)
- Multi-language UI (paraglide-js: English + Thai)
- Light/dark theme
- Optional worker/API split, Redis EventBus, DB-leased sharded workers

### Explicitly out of scope / not shipped

- ❌ Kafka monitor (heavy infra)
- ❌ RADIUS auth monitor (niche)
- ❌ Tauri desktop wrapper (web app only)
- ❌ Multi-tenant / multi-organization SaaS control plane

### On Hold (not roadmap commitments)

- **F3.4 application-managed custom-domain TLS/ACME** — internal deployments terminate TLS at the load balancer/Ingress layer.
- **Former enterprise/SaaS bundle** — multi-org tenancy, org roles/invites, compliance audit/export, Terraform provider, signed Grafana plugin, etc.
- **Additional auth hardening** — distributed WebAuthn challenge storage, session revocation, account lockout — only if a concrete threat model requires them.

## 3. Success Criteria

### MVP (Phase 1) — met

1. `helm install uptime-phoenix ./charts/uptime-phoenix` brings up a working monitoring tool with zero external dependencies (default: MariaDB or SQLite on a PVC + embedded static frontend in a single pod)
2. All 12 monitor types configurable via the UI and producing heartbeats
3. All 11 notification providers configurable and fire on status change
4. Real-time dashboard updates via WebSocket
5. Public status page with incident management
6. `/metrics` exposes Prometheus-format metrics behind API-key auth
7. Single-pod capacity target: 1,000 monitors at under 512 MB RAM (see also `docs/LOADTEST.md` for later scale numbers)

### Post-MVP internal deployment (Phase 5 focused) — met

1. Operator can enable OIDC and sign in through an internal IdP without a multi-tenant domain model
2. Local admin authentication remains usable for bootstrap and break-glass recovery
3. Versioned YAML configuration can be validated, dry-run, applied twice with no second-run changes, without exposing stored secrets
4. Uptime Phoenix operates behind an existing TLS-terminating load balancer/Ingress without an application-level certificate manager

### Scale (Sprint D) — met under documented host limits

- Heartbeat fan-out p95 under 1 s at 100, 1,000, and **10,000** monitors (split API/worker). Container side of the harness ran under **Colima 2 CPU / 4 GiB**, not full host RAM. See `docs/LOADTEST.md`.

## 4. Locked Decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | **Go 1.25+ backend** | Monitoring ecosystem is Go; ICMP native; single static binary (`go.mod` pins toolchain) |
| 2 | **Svelte 5 + SvelteKit frontend** | Fine-grained reactivity for real-time; smallest bundles |
| 3 | **Port-and-Adapter (hexagonal)** | Domain stays framework-free; monitor types and notification providers are swappable adapters; DB engine hot-pluggable |
| 4 | **MariaDB primary, SQLite for dev/edge** | MariaDB on a PVC (or external managed MariaDB) is the default for K8s; SQLite for local dev and single-node edge. Same repository interface, different adapter. A Postgres **app** DB adapter is not shipped; Postgres is a **monitor target** engine only. |
| 5 | **Bun query builder** | Type-safe queries for MariaDB + SQLite without ORM types in the domain. `go-sql-driver/mysql` + `modernc.org/sqlite` (CGO-free). ~~sqlc + pgx as app DB~~ was an early plan; not used. |
| 6 | **Echo v4 HTTP + coder/websocket** | Typed context, built-in middleware; idiomatic context-aware WS |
| 7 | **Embedded frontend by default, separate Deployment opt-in** | Single Go binary serves built Svelte assets via `embed.FS`. Split `phoenix-web` + `phoenix-api` via Helm when independent scaling is needed. |
| 8 | **Helm chart from day 1** | K8s is a first-class deployment target |
| 9 | **TOTP + WebAuthn** | `pquerna/otp` and `go-webauthn`; both shipped |
| 10 | **No external services required by default** | In-process EventBus + MariaDB/SQLite + embedded frontend. Redis pub/sub is opt-in for multi-pod. |
| 11 | **One-file plugin convention** | Each monitor type and notification provider is a single Go file implementing one interface |
| 12 | **EventBus port from day 1** | In-process default; Redis when `REDIS_URL` is set; domain code never changes |
| 13 | **TLS terminates at the K8s edge** | Cluster load balancer/Ingress owns certificates. Uptime Phoenix does not implement ACME in the current model. |
| 14 | **Focused Phase 5: SSO + config-as-code only** | Single-tenant, self-hosted. OIDC and GitOps configuration support internal operators; SaaS packaging does not. |

## 5. Key Risks

| Risk | Mitigation |
|---|---|
| ICMP requires elevated permissions on Linux | Document `sysctl net.ipv4.ping_group_range` + `setcap cap_net_raw`; provide fallback to TCP ping |
| WebSocket over K8s ingress kills long connections | Ingress annotations `proxy-read-timeout: 3600`; document for nginx/traefik |
| Single pod is SPOF in default deployment | PDB `minAvailable: 1`; liveness probe; fast restart; upgrade to multi-pod via Helm when HA is required |
| Svelte 5 runes learning curve | Reference the runes-based WebSocket store pattern in ARCHITECTURE.md |
| Partition management for heartbeats table | Partition CronJob; alert on partition count under 3 |
| Notification provider API changes | Each provider is one file; integration tests with mock servers |
| Load-test numbers vs real hardware | Document Colima/VM limits separately from host RAM (`docs/LOADTEST.md`) |

## 6. Team & Cadence

- **Size:** 1–2 developers (historically one full-stack hobby maintainer)
- **Cadence:** 2-week sprints while actively building; project is now hobby / not under active development (see README)
- **Testing:** unit tests for domain + ports; repository tests against MariaDB/SQLite; E2E with Playwright
- **CI + local gate:** GitHub Actions restored 2026-07-28 (`.github/workflows/ci.yml` on PR/main; `release.yml` for dry-run/publish). Local `make gate-full` remains required for thoroughness and works offline — see `docs/TESTING.md` and `docs/RELEASING.md`.

## 7. References

- `docs/ROADMAP.md` — phased delivery timeline and completion status
- `docs/ARCHITECTURE.md` — detailed technical design
- `docs/LOADTEST.md` — scale validation results
- `docs/TESTING.md` — gate commands and checklists
- `research/uptime-kuma.md` — original Uptime Kuma research
- `research/uptime-kuma-stack-alternatives.md` — stack comparison (historical research; Bun/MariaDB won)
- `research/uptime-kuma-k8s-architecture.md` — K8s deployment design
