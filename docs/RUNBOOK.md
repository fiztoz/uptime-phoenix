# Uptime Phoenix Operations Runbook

This runbook covers the current all-in-one and split deployments. Commands use
`https://phoenix.example.com` and a Helm release named `phoenix`; replace them with the
real values. Read `docs/DEPLOYMENT_MODES.md` before changing deployment shape.

## 1. Configuration backup and restore drill

Uptime Phoenix's backup API exports restorable application configuration. It includes proxy
passwords, notification tokens/webhooks, push tokens, and status-page password hashes.
Treat the file as a credential bundle: create it on an encrypted operator workstation,
restrict its permissions, never commit it, and delete it according to the secret-handling
policy after the drill.

It is not a full disaster-recovery backup. It is scoped to configuration owned by the
authenticated administrator; it does not aggregate other users' configuration. It also
does not contain users, API keys, passkey credentials, sessions, raw heartbeats, aggregate
history, or every install-wide record. Use a MariaDB backup/storage snapshot or a copy of
the SQLite database for full recovery.

### 1.1 Acquire an admin session token

The backup routes require a session JWT for an administrator. The following avoids putting
the password directly in the `curl` command line:

```bash
umask 077
export PHOENIX_URL=https://phoenix.example.com
read -r -p 'Admin username: ' PHOENIX_ADMIN_USERNAME
read -r -s -p 'Admin password: ' PHOENIX_ADMIN_PASSWORD
python3 - "$PHOENIX_ADMIN_USERNAME" "$PHOENIX_ADMIN_PASSWORD" > phoenix-login-request.json <<'PY'
import json
import sys

json.dump({"username": sys.argv[1], "password": sys.argv[2]}, sys.stdout)
PY
unset PHOENIX_ADMIN_PASSWORD

curl --fail-with-body --silent --show-error \
  -H 'Content-Type: application/json' \
  --data-binary @phoenix-login-request.json \
  "$PHOENIX_URL/api/auth/login" \
  -o phoenix-login-response.json
```

For an account without TOTP, extract the token:

```bash
export PHOENIX_TOKEN="$(python3 -c 'import json; print(json.load(open("phoenix-login-response.json"))["token"])')"
```

If the response contains `"requires_2fa": true`, exchange its ticket for a token:

```bash
read -r -p 'TOTP code: ' PHOENIX_TOTP_CODE
python3 - "$PHOENIX_TOTP_CODE" > phoenix-2fa-request.json <<'PY'
import json
import sys

with open("phoenix-login-response.json", encoding="utf-8") as source:
    ticket = json.load(source)["ticket"]
json.dump({"ticket": ticket, "token": sys.argv[1]}, sys.stdout)
PY
unset PHOENIX_TOTP_CODE

curl --fail-with-body --silent --show-error \
  -H 'Content-Type: application/json' \
  --data-binary @phoenix-2fa-request.json \
  "$PHOENIX_URL/api/auth/verify-2fa" \
  -o phoenix-session.json

export PHOENIX_TOKEN="$(python3 -c 'import json; print(json.load(open("phoenix-session.json"))["token"])')"
```

Remove the temporary login documents after the token has been extracted:

```bash
rm -f phoenix-login-request.json phoenix-login-response.json phoenix-2fa-request.json phoenix-session.json
```

### 1.2 Export and inspect

```bash
umask 077
curl --fail-with-body --silent --show-error \
  -H "Authorization: Bearer $PHOENIX_TOKEN" \
  "$PHOENIX_URL/api/backup/export" \
  -o phoenix-backup.json

python3 -m json.tool phoenix-backup.json > /dev/null
python3 - phoenix-backup.json <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    document = json.load(source)
print("version:", document["version"])
for field in (
    "proxies",
    "notifications",
    "tags",
    "monitor_groups",
    "monitors",
    "status_pages",
    "incidents",
    "maintenance_windows",
):
    print(f"{field}: {len(document.get(field, []))}")
PY
```

Copy `phoenix-backup.json` to a fresh, isolated Uptime Phoenix installation running the same or a
newer compatible release. Do not drill by importing it back into production: import is
merge-only and creates new entities rather than overwriting or deleting existing ones.

### 1.3 Import into the drill target

Set `PHOENIX_URL` and acquire `PHOENIX_TOKEN` for an administrator on the isolated target,
then run:

```bash
curl --fail-with-body --silent --show-error \
  -X POST \
  -H "Authorization: Bearer $PHOENIX_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @phoenix-backup.json \
  "$PHOENIX_URL/api/backup/import" \
  -o phoenix-import-summary.json

python3 -m json.tool phoenix-import-summary.json
python3 - phoenix-import-summary.json <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    summary = json.load(source)
skipped = summary.get("skipped", [])
if skipped:
    for item in skipped:
        print(
            f"SKIPPED {item.get('kind')}: "
            f"{item.get('name') or item.get('id') or '-'}: {item.get('reason')}"
        )
    raise SystemExit("restore drill incomplete: imported document contains skipped items")
print("restore drill import reported no skipped items")
PY
```

Open the target UI and effect-check at least one item of every exported kind: monitor target
and advanced fields, nested group placement, proxy credentials, monitor and folder
notification attachments, tags, status-page monitor order and access, incidents, and
maintenance assignments. A successful HTTP status alone does not prove the relationships
were restored.

Source IDs in the JSON are references, not stable destination IDs. Import creates fresh IDs
and remaps group parents, monitor groups/proxies, tags, notification links, status-page
links, incidents, and maintenance assignments. Tags may be reused by name and status-page
slugs may gain a suffix on collision. The status page's singular `custom_domain` field is
deliberately cleared on import to avoid uniqueness collisions; separately exported CNAME
aliases are recreated and may be reported as skipped. Compare content and relationships,
never numeric IDs. Review every entry in `skipped` before calling the drill successful.

## 2. Upgrade procedure

### 2.1 Before the rollout

1. Read the release notes and migration files between the deployed and target versions.
2. Take a full database backup or storage snapshot. The configuration export above is useful
   as a second artifact but is not a replacement for the database backup.
3. Record the current readiness and version:

```bash
export PHOENIX_URL=https://phoenix.example.com
curl --fail-with-body --silent --show-error "$PHOENIX_URL/api/health/ready"
curl --fail-with-body --silent --show-error "$PHOENIX_URL/api/health/live" | python3 -m json.tool
```

4. In split mode, verify MariaDB and Redis are healthy. Keep one migration-owning API pod
   during the schema transition; the migration runner is not concurrency-safe. Workers must
   remain behind the API readiness gate.

### 2.2 Pull and deploy

For Helm, pull the exact immutable image first so a missing tag fails before the rollout:

```bash
export PHOENIX_IMAGE=ghcr.io/fiztoz/phoenix
export PHOENIX_TAG=v0.0.0
docker pull "$PHOENIX_IMAGE:$PHOENIX_TAG"

helm upgrade uptime-phoenix ./charts/uptime-phoenix \
  --reuse-values \
  --set image.repository="$PHOENIX_IMAGE" \
  --set image.tag="$PHOENIX_TAG" \
  --set image.pullPolicy=Always

kubectl rollout status deployment \
  --selector=app.kubernetes.io/instance=phoenix \
  --timeout=5m
```

For the repository's build-based all-in-one Compose stack:

```bash
docker compose build --pull phoenix
docker compose up -d phoenix
docker compose logs --tail=100 phoenix
```

Uptime Phoenix applies every pending embedded `*.up.sql` migration during process startup, before
it reports ready. In the split Compose/Helm shape, start the API first; the worker readiness
gate waits for `/api/health/ready` before starting. Do not start multiple new migration
owners simultaneously.

### 2.3 Verify

```bash
curl --fail-with-body --retry 30 --retry-delay 2 --retry-all-errors \
  "$PHOENIX_URL/api/health/ready"

curl --fail-with-body --silent --show-error \
  "$PHOENIX_URL/api/health/live" \
  -o phoenix-health-live.json

export PHOENIX_EXPECTED_VERSION="${PHOENIX_TAG:-dev}"
python3 - "$PHOENIX_EXPECTED_VERSION" <<'PY'
import json
import sys

with open("phoenix-health-live.json", encoding="utf-8") as source:
    live = json.load(source)
print(json.dumps(live, indent=2))
if live.get("status") != "alive":
    raise SystemExit("liveness status is not alive")
if live.get("version") != sys.argv[1]:
    raise SystemExit(
        f"version mismatch: expected {sys.argv[1]!r}, got {live.get('version')!r}"
    )
PY
```

Set `PHOENIX_EXPECTED_VERSION` to the exact build stamp if it differs from the image tag.
An unstamped local build reports `"version": "dev"`; release images must be built with the
version ldflag. After the health checks, log in, verify the dashboard receives new
heartbeats over WebSocket, and run one known-good monitor and notification test-send.

## 3. Migration rollback policy

Uptime Phoenix's runtime migration runner reads and applies sorted `*.up.sql` files and records
them in `_migrations`. It does not execute `*.down.sql`. Down files exist for development
and review, but the project does not continuously exercise the complete production
downgrade chain; they are not the supported incident rollback path.

If a migration or new binary fails:

1. Stop the rollout and prevent additional new-version processes from starting.
2. Preserve application and database logs and identify the last migration recorded in
   `_migrations`.
3. Restore the pre-upgrade database backup/snapshot as a unit.
4. Redeploy the previous known-good image against the restored database.
5. Verify readiness, the previous version field, login, heartbeat recording, and live
   WebSocket updates before reopening traffic.

Never run a `.down.sql` file manually against production, delete a row from `_migrations`,
or mix an old binary with a partially upgraded schema. A forward repair migration may be
chosen after incident review, but restore-from-backup is the supported rollback.

## 4. Split-mode scaling

| Concern | Operating rule |
|---|---|
| Process role | `MODE=all` runs HTTP/WS and workers together; `MODE=api` is HTTP/WS only; `MODE=worker` is scheduler/checker/notifier only. `cmd/api` and `cmd/worker` hard-code their roles. |
| Database | Split processes require the same MariaDB. SQLite is single-process only. Include `parseTime=true`, `loc=UTC`, and `multiStatements=true` in a MariaDB DSN. |
| Event bus | Set the same `REDIS_URL` on every API and worker. Without Redis, each process gets its own in-memory bus: heartbeats persist, but API WebSocket clients do not receive worker events. |
| API scaling | API replicas are stateless with respect to monitor data. Put them behind one Service/Ingress and retain WebSocket upgrade/timeouts. The rolling-restart soak is **validated** — see §4.1: clients reconnect using the frontend's normal backoff and lose no heartbeats, because the worker never stops writing to the DB while an API pod restarts. |
| Worker sharding | Give every worker a unique non-empty `WORKER_ID`. That selects the DB-leased scheduler. Set `SHARD_BATCH_SIZE`, `SHARD_LEASE_TTL`, and `SHARD_POLL_EVERY` consistently. Keep poll interval comfortably below lease TTL. |
| Lease failure | Graceful shutdown releases the worker's leases. After a crash, another worker can claim them only after `SHARD_LEASE_TTL`; reduce TTL carefully because slow checks still need ownership stability. |
| Migration order | The API must migrate first. In the supplied split stack, `api-ready-gate` blocks workers; preserve that ordering in custom deployments. |

The Helm chart enables worker IDs and lease settings with:

```bash
helm upgrade uptime-phoenix ./charts/uptime-phoenix --reuse-values \
  --set mode=split \
  --set database.engine=mariadb \
  --set redis.enabled=true \
  --set worker.replicas=3 \
  --set worker.shards.enabled=true
```

Monitor lease distribution and check throughput before increasing replicas. More API pods
do not increase checker capacity; more sharded worker pods do.

### 4.1 Rolling-deploy WebSocket soak (Sprint D Track C, 2026-07-25)

This closes the "rolling deploy test" carried since Phase 3 Sprint 7 (see `docs/ROADMAP.md`)
and the Sprint B assurance backlog: prove that restarting `phoenix-api` while
`phoenix-worker` keeps running does not lose heartbeats and that WebSocket clients
reconnect and resume on their own. No production code changed for this — it is a
black-box soak against the existing binaries, run twice, with the durable heartbeat log
used as the ground truth.

**Why this definition of "no event loss":** the WebSocket hub is a *live push* channel,
not the system of record — heartbeats are written to MariaDB by the worker regardless of
whether any client is connected to receive them live. So "no event loss" is verified two
ways: (1) the heartbeat table has no gap or duplicate across the restart window (the
worker never stopped, so nothing is lost from the durable record), and (2) the WS client
reconnects on its own and its rehydrated state (`monitor.list` on connect, in
`internal/adapters/ws/hub.go`) matches that ground truth. An individual live *push* during
the socket's downtime cannot be redelivered after the fact (the socket didn't exist) —
that is physically unavoidable during any restart, on any system — but nothing is lost
from the record a client resyncs against.

**Reproducible procedure:**

1. Build `cmd/api` and `cmd/worker` (`go build -o bin/phoenix-api ./cmd/api`,
   `go build -o bin/phoenix-worker ./cmd/worker`).
2. Start a throwaway MariaDB + Redis (do **not** reuse the shared dev `phoenix` database
   or its containers):
   ```bash
   docker run -d --name phx-soak-mariadb \
     -e MARIADB_ROOT_PASSWORD=soak_root -e MARIADB_DATABASE=phoenix_soak \
     -e MARIADB_USER=phoenix_soak -e MARIADB_PASSWORD=soak_pw \
     -p 127.0.0.1:13406:3306 mariadb:11
   docker run -d --name phx-soak-redis -p 127.0.0.1:16380:6379 redis:7-alpine
   ```
3. Start the worker once and the API (both pointed at the same `DB_DSN` and
   `REDIS_URL=redis://127.0.0.1:16380/0`, `DB_ENGINE=mariadb`). The worker is never
   restarted for the rest of the soak — only the API is.
4. Create one fast-interval HTTP monitor (`interval=2`, `retry_interval=2`,
   `max_retries=1`) via `POST /api/monitors` so heartbeats and status changes are dense.
5. Connect a WebSocket client to `GET /ws?token=<jwt>` using the **same reconnect
   contract as the real frontend store** (`web/src/lib/stores/ws.svelte.ts`): on close,
   back off and retry (first attempt after 1s). Log every frame with a wall-clock
   timestamp and a running sequence number.
6. Send `SIGTERM` to the API process (graceful shutdown — matches a K8s rolling update)
   while the worker keeps running, then start a new API process on the same port.
7. After the soak window, fetch `GET /api/monitors/:id/heartbeats` and check the `id`
   sequence for gaps or duplicates across the restart window; check the client's log for
   dropped/duplicated/out-of-order sequence numbers and for a successful reconnect.

**Measured result (two trials, both split mode, MariaDB + Redis, one HTTP monitor
checking its own API's `/api/health/live` every 2s):**

| Trial | API downtime (port closed → new process listening) | Client-observed gap (WS close → WS reopen) | Heartbeat log | Client behavior |
|---|---|---|---|---|
| 1 — stress case (~21s manual outage between kill and restart) | ~21.1s | ~21.1s (client retried on a fixed 1s interval throughout) | **Continuous, no gaps**: ids 6→65 unbroken; correctly recorded `up`→`pending`→`down`×7→`up` (retry-confirm) for the whole outage | Reconnected on the first attempt once the port was live; `monitor.list` on reconnect correctly showed the monitor `down` (accurate as of that instant), then a fresh `up` heartbeat + `status.change` + `stats.update` arrived within 1.1s of the target becoming reachable again |
| 2 — realistic fast rolling restart (scripted, no manual gap) | **≈16ms** (`port free` → new process's `http server listening` log line) | **1001ms**, entirely the client's first backoff step (`Math.min(1000 * 2**0, 30000)`), not server unavailability | **Continuous, no gaps**: ids 82→111 unbroken, monitor never observed `down` (outage shorter than one 2s check interval) | Reconnected on the first attempt; live heartbeat stream resumed with no duplicate or missing sequence numbers |

**Conclusion:** the rolling-restart no-loss guarantee holds. The durable heartbeat record
never has a gap because the worker is untouched by an API restart (this is the entire
point of the split architecture); the frontend's existing exponential-backoff reconnect
(already shipped, unmodified) resumes the live view within about one second of the API
actually being reachable again, and rehydrates via the existing `sendMonitorList`
on-connect snapshot rather than requiring individual missed pushes to be replayed. Under
a normal, fast rolling update (trial 2), a user would perceive under 1.5 seconds of stale
dashboard data and zero missing history. Under an abnormally long outage (trial 1, ~21s,
well beyond any real deployment), the guarantee still holds — it only takes longer for the
client to notice, and it noticed within its own retry cadence.

**One finding reported, not fixed (Track C does not edit `internal/adapters/ws/` — that
code is owned by Track A, in flight concurrently):** every graceful API shutdown observed
in both trials logged 6–8 lines of `WARN ws hub: dropping event with no resolvable
monitor id type=""` in immediate succession around the `"shutdown signal received"` /
`"phoenix stopped gracefully"` log lines. This reproduced on every trial. It does not
appear to cause data loss — the hub's fail-closed drop behavior (`hub.go`'s `broadcast`)
is doing exactly what the hub's fail-closed drop path is designed to do — but it looks
like a handful of zero-value `ports.Event{}` structs are being pushed through the Redis
subscriber's channel as it closes during teardown, which is unnecessary log noise on
every single rolling restart and could mask a real warning in aggregated logs. Still
open noise; does not affect heartbeat continuity (see soak results above).

## 5. Environment variable reference

The following defaults are the `internal/bootstrap/config.go` struct-tag defaults.

| Variable | Default | Meaning / production rule |
|---|---|---|
| `PORT` | `3000` | HTTP listen port. |
| `HOST` | `0.0.0.0` | HTTP bind host. |
| `LOG_LEVEL` | `info` | Application log level. |
| `DB_ENGINE` | `sqlite` | `sqlite` or `mariadb`. Split mode requires MariaDB. |
| `DB_DSN` | `file:phoenix.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)` | Engine-specific DSN. MariaDB migrations require `multiStatements=true`; use `parseTime=true&loc=UTC`. |
| `JWT_SECRET` | `change-me-in-production` | Session-signing secret. Replace with a stable secret before first production start. |
| `JWT_EXPIRE_HOURS` | `24` | Session JWT lifetime in hours. |
| `TOTP_ISSUER` | `Phoenix` | Issuer label shown in authenticator apps. |
| `PRODUCTION` | `false` | Selects secure production defaults, including deny-by-default CORS when no allow-list is supplied. |
| `REDIS_URL` | empty | Optional Redis URL. Required and shared across processes in split mode. |
| `WEBAUTHN_RP_ID` | `localhost` | Passkey relying-party ID; use the registrable production hostname. |
| `WEBAUTHN_RP_NAME` | `Phoenix` | Passkey relying-party display name. |
| `WEBAUTHN_RP_ORIGINS` | `http://localhost:3000,http://localhost:5173` | Comma-separated exact browser origins, including scheme. |
| `BOOTSTRAP_USERNAME` | empty | Creates the first user only when paired with `BOOTSTRAP_PASSWORD`; do not use as an ongoing user-sync mechanism. |
| `BOOTSTRAP_PASSWORD` | empty | First-user bootstrap password; minimum eight characters. Supply via a secret and remove from runtime configuration after bootstrap. |
| `MODE` | `all` | `all`, `api`, or `worker` for `cmd/app`; role-specific binaries override it. |
| `CORS_ALLOW_ORIGINS` | empty | Comma-separated full origins. Empty + `PRODUCTION=false` allows `*`; empty + `PRODUCTION=true` emits no CORS grant. Same-origin requests are unaffected. |
| `WS_ALLOWED_ORIGINS` | empty | Extra WebSocket origin **host patterns without schemes**, comma-separated, in addition to secure same-origin access; for example `status.example.com,localhost:5173`. |
| `WS_ALLOW_ANY_ORIGIN` | `false` | Disables WebSocket origin verification. Development escape hatch only; never enable in production. |
| `RATE_LIMIT_RPS` | `50` | API ingress requests per second. Health endpoints are excluded. |
| `RATE_LIMIT_BURST` | `100` | API ingress token-bucket burst. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty | Enables optional OTLP HTTP export when set. |
| `OTEL_SERVICE_NAME` | `phoenix` | OpenTelemetry service name. |
| `WORKER_ID` | empty | Non-empty unique ID selects the sharded, DB-leased scheduler. Empty selects the local scheduler. |
| `SHARD_BATCH_SIZE` | `200` | Maximum monitors claimed by one sharded worker. |
| `SHARD_LEASE_TTL` | `300` | Lease lifetime in seconds. |
| `SHARD_POLL_EVERY` | `30` | Claim/refresh interval in seconds. |
| `HEARTBEAT_RETENTION_DAYS` | `180` | Raw-heartbeat retention. Set `0` to disable retention deletion. |
| `PUBLIC_URL` | empty | Absolute public origin for subscription links and OIDC post-login redirects. |
| `OIDC_ISSUER` | empty | OIDC issuer URL. Empty disables SSO. |
| `OIDC_CLIENT_ID` | empty | OIDC client ID (required when issuer is set). |
| `OIDC_CLIENT_SECRET` | empty | OIDC client secret — supply via Kubernetes Secret. |
| `OIDC_REDIRECT_URL` | empty | Absolute callback URL. When empty, derived from `PUBLIC_URL` + `/api/auth/oidc/callback`. |
| `OIDC_SCOPES` | `openid,profile,email` | Comma-separated OIDC scopes. |
| `OIDC_GROUPS_CLAIM` | `groups` | ID-token/userinfo claim for group membership. |
| `OIDC_JIT_ENABLED` | `true` | Create Uptime Phoenix users on first successful OIDC login. |
| `OIDC_LINK_BY_EMAIL` | `false` | Link an unlinked existing user when the IdP asserts a **verified** email matching the username. |
| `OIDC_ALLOWED_GROUPS` | empty | When non-empty, require membership in at least one listed IdP group. |
| `OIDC_ADMIN_GROUPS` | empty | IdP groups that set `is_admin` on every login. |
| `OIDC_CAP_*_GROUPS` | empty | IdP groups for capability flags (`NOTIFICATIONS`, `MAINTENANCE`, `CREATE_MONITORS`, `CREATE_GROUPS`). |
| `OIDC_GRANT_MAP` | empty | Scoped grants: `idp-group:group:<id>,idp-team:monitor:<id>` (optional `:shallow`). |

### OIDC SSO (break-glass and IdP outage)

When OIDC is enabled, **local password + TOTP/passkey login stays available**. Keep at least
one admin account with a strong local password (and TOTP) for recovery:

1. **IdP outage** — operators sign in at `/login` with the break-glass local account. SSO button
   will fail until the IdP recovers; local login is independent.
2. **Lost admin via group sync** — if `OIDC_ADMIN_GROUPS` is set, losing that IdP group removes
   `is_admin` on the next SSO login. Local login does **not** re-sync groups, so a local admin
   password remains the recovery path. Update IdP membership or use local login to promote
   another admin via `/api/users`.
3. **JIT disabled** — set `OIDC_JIT_ENABLED=false` and pre-create users; optional
   `OIDC_LINK_BY_EMAIL=true` links by verified email once. Unknown subjects get
   `no_account` on the login page.
4. **Callback URL** — register `{PUBLIC_URL}/api/auth/oidc/callback` (or the explicit
   `OIDC_REDIRECT_URL`) with the IdP. Multi-pod API works without Redis: state is HMAC-signed
   with `JWT_SECRET`.

Tests: `internal/core/services/auth_oidc*_test.go`. Local agent contracts (gitignored):
`docs/local/F5-S13-OIDC-CONTRACTS.md`.

### Config-as-code (GitOps)

Declarative YAML for monitors, groups, tags, notifications, maintenance, and status
pages. Stable keys (not DB IDs). Admin-only endpoints:

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/config/export` | Redacted YAML of every **keyed** resource |
| POST | `/api/config/validate` | Schema + ref check, no writes |
| POST | `/api/config/plan` | Dry-run diff (`{document, prune?}`) |
| POST | `/api/config/apply` | Upsert; optional `prune: true` |

Example document: `examples/config/phoenix-config.example.yaml`.

#### CLI (`cmd/phoenix-config`)

Small HTTP client (no server embed). Auth via admin session JWT (`PHOENIX_TOKEN` /
`--token`) or write-scoped API key (`PHOENIX_API_KEY` / `--api-key`). Base URL:
`PHOENIX_URL` / `--url` (default `http://127.0.0.1:3000`).

```bash
export PHOENIX_URL=http://127.0.0.1:3000
export PHOENIX_TOKEN=...   # or: export PHOENIX_API_KEY=phx_...

go run ./cmd/phoenix-config validate --file examples/config/phoenix-config.example.yaml
go run ./cmd/phoenix-config plan     --file examples/config/phoenix-config.example.yaml
go run ./cmd/phoenix-config plan     --file examples/config/phoenix-config.example.yaml --prune
go run ./cmd/phoenix-config apply    --file examples/config/phoenix-config.example.yaml --yes
go run ./cmd/phoenix-config export   --out /secure/path/phoenix-config.yaml   # mode 0600
```

`apply` requires `--yes`. Plan/apply wrap the file as `{document, prune}` for the API.
HTTP errors print status + body on stderr; credentials are never logged.

#### curl

```bash
# Validate then apply (session JWT or write API key of an admin)
curl -sS -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/yaml" \
  --data-binary @examples/config/phoenix-config.example.yaml \
  "$PHOENIX_URL/api/config/validate"
curl -sS -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/yaml" \
  --data-binary @examples/config/phoenix-config.example.yaml \
  "$PHOENIX_URL/api/config/apply"
```

Rules worth remembering:

1. Applying the same document twice is a no-op on the second run.
2. Export uses `__REDACTED__` for secrets; re-applying that export preserves stored secrets.
3. Prune only deletes resources that already have a `config_keys` row. Admin-UI resources
   without keys are never pruned.
4. Full secret-bearing restore remains `/api/backup/*`. Config-as-code is the GitOps path.
5. Rollback: re-apply a previous YAML from Git, or restore via backup/import.

Tests: `internal/core/services/configascode_test.go`. Local agent contracts (gitignored):
`docs/local/F5-S14-CONFIG-AS-CODE-CONTRACTS.md`.

## 6. Troubleshooting

### Dashboard data loads but WebSocket status does not update

1. Inspect the browser's WebSocket handshake. An origin refusal is normally a 403/failed
   upgrade at `/ws?token=...`.
2. Same-origin is allowed by default. If a reverse proxy or Vite serves the browser from a
   different host/port, add that host pattern to `WS_ALLOWED_ORIGINS` without `http://` or
   `https://`, then restart the API pods.
3. Do not try to fix a WebSocket refusal with `CORS_ALLOW_ORIGINS`; CORS and WebSocket
   origin checks are separate.
4. In split mode, verify every API and worker uses the same `REDIS_URL` and inspect Redis
   connectivity. Database heartbeats with no live events usually indicate a missing or
   disconnected cross-process event bus.
5. Use `WS_ALLOW_ANY_ORIGIN=true` only to isolate a local-development origin problem, then
   turn it back off and configure the allow-list.

### MariaDB reports `ERROR 1292 Incorrect datetime value: '0001-01-01...'`

This means a Go zero `time.Time` crossed the MariaDB boundary. SQLite may accept the same
write, so reproducing only with SQLite is not sufficient. Current repositories normalize
creation timestamps and map unused cron-maintenance start/end dates to `NULL`; a recurrence
is a regression or an unnormalized new field.

Capture the failing entity and statement, verify the running build includes all migrations,
and reproduce it with the MariaDB repository suite. Do not disable strict SQL mode or enable
zero dates as a workaround. Fix the repository/service boundary so valid UTC values or
`NULL` reach MariaDB.

### Frontend changes do not appear

The all-in-one frontend is compiled to `web/dist` and embedded into the Go binary with
`go:embed`. Rebuilding only the Svelte files does not change an already-built Uptime Phoenix
binary. Run the frontend build, then rebuild/redeploy the Go binary or the root Docker image:

```bash
cd web
bun run build
cd ..
go build ./cmd/app
```

In split-web mode, rebuild and redeploy the `phoenix-web` image instead. If `/api/health/live`
shows the expected backend version but the UI is stale, confirm which web tier the Ingress
serves and whether its image digest changed.

### Readiness stays unavailable after an upgrade

`/api/health/ready` returns 503 when its database ping fails. Check database reachability,
credentials, and `DB_ENGINE`/`DB_DSN`; for MariaDB, confirm the database exists and the DSN
uses `parseTime=true&loc=UTC&multiStatements=true`. Then inspect startup logs for the exact
migration filename and statement. Do not start workers until the API is ready.
