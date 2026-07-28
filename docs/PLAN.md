# Phoenix — Project Plan

> A self-hosted, K8s-native monitoring tool. Uptime-Kuma-equivalent features, Port-and-Adapter architecture, Go + Svelte 5 stack.

---

## 1. Goal

Build a self-hosted monitoring tool that matches Uptime Kuma's feature surface (uptime checks, notifications, public status pages, real-time dashboard) while being:

- **Minimal-dependency by default** — one pod, one PVC, zero external services required to function. `helm install` → one pod → works. Postgres, Redis, and the separate web tier are **opt-in**, never required.
- **K8s-native from day 1** — ships as a Helm chart; horizontal scaling is a config change, not a rewrite
- **Single-tenant and self-hosted** — each installation is owned and operated by the team deploying it; Phoenix is not a shared SaaS control plane
- **Platform-owned edge TLS** — Phoenix runs behind the cluster's load balancer/Ingress; the platform terminates and renews certificates
- **Frontend/backend separable** — same binary serves embedded static assets by default; split into independent Deployments when you need scale
- **Port-and-Adapter (hexagonal) architecture** — domain logic independent of frameworks, databases, and transports
- **Lean** — no required heavy infrastructure dependencies (no Kafka, RabbitMQ, message brokers, or external DB operators needed for Phoenix itself)

## 2. Scope

### In Scope (Phase 1 — MVP)

**Monitor types (12, all opt-in/lightweight in Phoenix itself):**
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
- Database connect ping (PostgreSQL, MySQL, MongoDB, Redis)

**Notification providers (12, all HTTP-based):**
- Telegram, Discord, Slack, Email/SMTP, Generic Webhook, ntfy, Pushover, Microsoft Teams, Mattermost, PagerDuty, OpsGenie, Gotify

**Core features:**
- Real-time dashboard (single persistent WebSocket, Svelte 5 runes)
- Public status pages (SvelteKit SSG, custom domain mapping)
- Tags + monitor grouping
- Maintenance windows (single + cron strategies)
- 2FA (TOTP only for early phase)
- API keys for Prometheus `/metrics` endpoint
- Multi-language UI (paraglide-js)
- Light/dark theme

### In Scope (Post-MVP — Internal K8s Integration)

- **OIDC SSO** — integrate with an internal identity provider and map identities/groups onto
  Phoenix's existing admin, capability, and scoped-monitor permission model. Local authentication
  remains available for bootstrap and break-glass access.
- **Declarative config-as-code** — versioned YAML for monitors, groups, tags, notifications,
  maintenance windows, and status pages; support a dry-run and idempotent apply suitable for
  GitOps. Export must not reveal secrets, omitted secret fields must preserve existing values,
  and deletion requires an explicit prune operation. Runtime and OIDC credentials remain in
  Helm values/environment variables backed by Kubernetes Secrets.
- **Ingress integration documentation** — document the required forwarded headers, WebSocket
  settings, and TLS assumptions for clusters where the load balancer/Ingress owns HTTPS.

### Out of Scope (explicitly dropped for leanness)

- ❌ Kafka monitor (heavy infra)
- ❌ RADIUS auth monitor (niche)
- ❌ MSSQL connect monitor (niche — Postgres + MySQL + MongoDB + Redis cover the common cases)
- ❌ WebAuthn / passkeys (TOTP only for early phase; interface reserved for later)
- ❌ Tauri desktop wrapper (web app only for early phase)
- ❌ Distributed/sharded workers (Phase 3+; Phase 1 is single worker)

### Deferred (Phase 2+, architecture supports it — all opt-in, none required)

- External Postgres (SQLite is the default; Postgres when you outgrow a single node)
- Redis pub/sub for cross-pod event fan-out (only when running multiple API pods)
- Worker/API split into separate Deployments (only when single-pod capacity is exhausted)
- Separate `phoenix-web` Deployment (single binary serves embedded assets by default)
- Sharded workers via DB lease (only at 50k+ monitors)
- WebAuthn 2FA
- Tauri wrapper

### On Hold (not roadmap commitments)

- **F3.4 application-managed custom-domain TLS/ACME** — internal deployments terminate TLS at
  the load balancer/Ingress layer. Phoenix will not store ACME account keys or manage
  certificates unless a future deployment requirement proves this is necessary.
- **The former Phase 5 enterprise/SaaS bundle** — multi-organization tenancy, organization
  roles and invitations, tenant-isolation migrations, compliance audit/export features,
  per-tenant retention/deletion, a Terraform provider, scheduled SLO reports, and a signed
  Grafana plugin. These remain possible backlog ideas, not scheduled work.
- **Additional authentication hardening from the former Phase 5** — distributed WebAuthn
  challenge storage, session revocation, and account lockout. Reconsider only when an observed
  deployment or threat-model requirement justifies them.

## 3. Success Criteria

The MVP is complete when:

1. `helm install phoenix ./charts/phoenix` brings up a working monitoring tool in <2 minutes on any K8s cluster **with zero external dependencies** — no Postgres, no Redis, no message broker required; SQLite on a PVC + embedded static frontend in a single pod
2. All 12 monitor types can be configured via the UI and produce heartbeats
3. All 12 notification providers can be configured and fire on status change
4. The real-time dashboard updates via WebSocket with <500ms latency
5. A public status page renders at a custom domain with incident management
6. `/metrics` exposes Prometheus-format metrics behind API-key auth
7. The Docker image is <30 MB (Go) + <10 MB (web)
8. A single pod handles 1,000 monitors with 60s intervals at <512 MB RAM

The focused post-MVP internal-deployment work is complete when:

1. An operator can enable OIDC and sign in through the internal IdP without introducing an
   organization/multi-tenant domain model.
2. Local admin authentication remains usable for bootstrap and break-glass recovery.
3. A versioned YAML configuration can be validated, dry-run, applied twice with no second-run
   changes, and reconciled without exposing stored secrets.
4. Phoenix operates behind an existing TLS-terminating load balancer/Ingress without requiring
   an application-level certificate manager.

## 4. Locked Decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | **Go 1.23+ backend** | Monitoring ecosystem is Go; ICMP native; single static binary; 2× faster dev than Rust |
| 2 | **Svelte 5 + SvelteKit frontend** | Fine-grained reactivity for real-time; smallest bundles; 60fps under stress |
| 3 | **Port-and-Adapter (hexagonal)** | Domain stays framework-free; monitor types and notification providers are swappable adapters; DB engine hot-pluggable |
| 4 | **MariaDB primary, SQLite for dev/edge** | MariaDB on a PVC (or external managed MariaDB) is the default for K8s; SQLite for local dev and single-node edge. Same repository interface, different adapter. Postgres supported as a third adapter if demand exists. |
| 5 | **sqlc + pgx** | Type-safe SQL, no ORM runtime overhead, domain stays SQL-pure |
| 6 | **Echo v4 HTTP + coder/websocket** | Typed context, built-in middleware; idiomatic context-aware WS |
| 6a | **Bun (SQL-first query builder) for MariaDB + SQLite** | sqlc is Postgres-centric; Bun generates type-safe queries for both MariaDB and SQLite from the same codebase. Aligns with hexagonal — domain stays free of ORM types. `go-sql-driver/mysql` for MariaDB, `modernc.org/sqlite` for SQLite (CGO-free). |
| 7 | **Embedded frontend by default, separate Deployment opt-in** | Single Go binary serves the built Svelte assets via `embed.FS` by default (zero-dependency). Split into `phoenix-web` (nginx) + `phoenix-api` Deployments via Helm value when independent scaling is needed. |
| 8 | **Helm chart from day 1** | K8s is a first-class deployment target, not an afterthought |
| 9 | **TOTP only (early phase)** | `pquerna/otp`; WebAuthn port reserved for later |
| 10 | **No external services required by default** | In-process EventBus + SQLite + embedded frontend = zero external dependencies. Redis pub/sub is an opt-in adapter for multi-pod deployments only. |
| 11 | **One-file plugin convention** | Each monitor type and notification provider is a single Go file implementing one interface |
| 12 | **EventBus port from day 1** | In-process impl in Phase 1; Redis impl in Phase 2; domain code never changes |
| 13 | **TLS terminates at the K8s edge** | The cluster load balancer/Ingress owns certificates and renewal. Phoenix serves HTTP behind it and does not implement ACME in the current deployment model. |
| 14 | **Focused Phase 5: SSO + config-as-code only** | Phoenix is a single-tenant, self-hosted project. OIDC and GitOps configuration directly support internal operators; SaaS and enterprise packaging do not. |

## 5. Key Risks

| Risk | Mitigation |
|---|---|
| ICMP requires elevated permissions on Linux | Document `sysctl net.ipv4.ping_group_range` + `setcap cap_net_raw`; provide fallback to TCP ping |
| WebSocket over K8s ingress kills long connections | Ingress annotations `proxy-read-timeout: 3600`; document for nginx/traefik |
| Single pod is SPOF in default deployment | PDB `minAvailable: 1`; liveness probe; fast restart (static binary, <2s startup); this is the deliberate tradeoff for zero-dependency simplicity — upgrade to multi-pod via Helm values when HA is required |
| Svelte 5 runes learning curve | Reference the runes-based WebSocket store pattern in ARCHITECTURE.md; one developer owns the frontend |
| Partition management for heartbeats table | `create_heartbeats_partition()` cron function; alert on partition count <3 |
| Notification provider API changes | Each provider is one file; easy to update; integration tests with mock servers |

## 6. Team & Cadence

- **Size:** 1–2 developers (1 Go-fluent backend + 1 Svelte-fluent frontend, or one full-stack)
- **Cadence:** 2-week sprints; demo at end of each phase
- **Testing:** unit tests for domain + ports; integration tests with testcontainers-go; E2E with Playwright
- **CI:** none — `.github/` was deliberately removed (`9de75e9`, 2026-07-25) and stays
  gone. The gate below runs locally instead, via `make gate-full`
  (`golangci-lint` + `go test -race` + `bun run lint`/`check`/`test`/`build` + Playwright
  e2e + `helm lint`/`template` + `govulncheck`), triggered by a human before every merge —
  see `docs/TESTING.md` and `docs/RELEASING.md`.

## 7. References

- `docs/ROADMAP.md` — phased delivery timeline
- `docs/ARCHITECTURE.md` — detailed technical design
- `research/uptime-kuma.md` — original Uptime Kuma research
- `research/uptime-kuma-stack-alternatives.md` — stack comparison
- `research/uptime-kuma-k8s-architecture.md` — K8s deployment design
