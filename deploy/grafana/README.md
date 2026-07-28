# Phoenix — Grafana Integration

Visualise Phoenix's Prometheus metrics in Grafana. Rather than ship a bespoke
Grafana backend datasource plugin (high maintenance, needs signing), Phoenix
provides the idiomatic deliverable: a **provisioned Prometheus data source +
curated dashboards + a one-command stack**.

Everything here lives under `deploy/grafana/` and is self-contained.

```
deploy/grafana/
├── docker-compose.yaml                       # Prometheus + Grafana, fully provisioned
├── prometheus.yml                            # Prometheus config used by the compose stack
├── prometheus-scrape.yaml                    # copy/paste scrape_config snippet for an existing Prometheus
├── provisioning/
│   ├── datasources/phoenix.yaml              # Grafana data source (Prometheus)
│   └── dashboards/phoenix.yaml               # dashboard provider (loads ./dashboards)
└── dashboards/
    └── phoenix-overview.json                 # importable "Phoenix — Overview" dashboard
```

---

## Metrics this integration uses

All panels are built **only** from metrics Phoenix actually registers in
`internal/adapters/metrics/prometheus.go`:

| Metric | Type | Labels | Used by |
|---|---|---|---|
| `phoenix_monitor_status` | gauge | `monitor_id`, `monitor_name`, `type` | UP/DOWN stats, status timeline, status table |
| `phoenix_monitor_latency_ms` | gauge | `monitor_id`, `monitor_name` | Latency time series |
| `phoenix_heartbeats_total` | counter | `monitor_id`, `status` | Heartbeats/sec stat, heartbeat-rate-by-status series |
| `phoenix_monitors_active` | gauge | _(none)_ | Active Monitors stat |
| `phoenix_notifications_sent_total` | counter | `provider`, `status` | _(available, not on this dashboard)_ |
| `phoenix_ws_connections_active` | gauge | _(none)_ | _(available, not on this dashboard)_ |

`phoenix_monitor_status` values: **0 = DOWN, 1 = UP, 2 = PENDING, 3 = MAINTENANCE**.

The dashboard uses a `$datasource` (Prometheus) template variable and a
`$monitor` variable populated from
`label_values(phoenix_monitor_status, monitor_name)`. No datasource UID is
hard-coded — every panel references `${datasource}`.

---

## Authentication: how `/metrics` is protected

Phoenix's `/metrics` route is guarded by the API-key middleware
(`internal/adapters/http/middleware/apikey.go`). It accepts:

- **`Authorization: ApiKey <plaintext>`** — the middleware takes the plaintext,
  SHA-256-hashes it, and looks the hash up. **This is the supported path.**
- A `Basic ` branch exists but uses the **raw base64 blob** after `Basic ` as the
  key *without decoding it*, so a normal HTTP Basic Auth client (which sends
  `base64("user:pass")`) will **not** match the stored hash. Use the `ApiKey`
  form instead. The `basic_auth` snippets in the config files are included only
  for completeness and are commented out / marked as non-working.

### Get a Phoenix API key

A key looks like `phx_<base64url>`. Mint one against a running Phoenix:

```bash
# 1. Log in to get a session JWT (or grab it from the browser after logging in).
# 2. Create the key (the plaintext is returned ONCE — store it safely):
curl -s -X POST http://localhost:3000/api/api-keys \
     -H "Authorization: Bearer <session-jwt>" \
     -H "Content-Type: application/json" \
     -d '{"name":"prometheus-scrape","scopes":["metrics"]}'
```

Verify it works:

```bash
curl -s -H "Authorization: ApiKey phx_xxxxx" http://localhost:3000/metrics | head
```

---

## Two topologies

### 1. Prometheus-in-the-middle (recommended)

Prometheus scrapes Phoenix `/metrics` using the API key; Grafana points at
Prometheus. Grafana never holds the Phoenix key, and you get retention,
recording rules, and alerting for free. **This is what the bundled
`docker-compose.yaml` sets up**, and the primary entry in
`provisioning/datasources/phoenix.yaml`.

```
Grafana ──query──▶ Prometheus ──scrape (ApiKey)──▶ Phoenix /metrics
```

### 2. Direct scrape (no separate Prometheus)

Grafana's Prometheus data source points straight at Phoenix. Documented (and
left commented) in `provisioning/datasources/phoenix.yaml`. Note Phoenix only
exposes the raw `/metrics` exposition endpoint — it is not a full Prometheus
query API — so direct mode is best treated as a stop-gap; prefer topology 1.

---

## Quickstart (docker compose)

From `deploy/grafana/`:

```bash
# Phoenix must be running and reachable (default: http://localhost:3000).
PHOENIX_API_KEY=phx_xxxxx docker compose up
```

Then:

- **Grafana** → http://localhost:3001 (login `admin` / `admin`; anonymous
  viewer is also enabled). Open **Dashboards → Phoenix → Phoenix — Overview**.
- **Prometheus** → http://localhost:9090 (check **Status → Targets**; the
  `phoenix` target should be `UP`).

The compose stack writes `PHOENIX_API_KEY` into a credentials file that
Prometheus reads via `credentials_file`, so the key never appears in the
mounted config. The default scrape target is `host.docker.internal:3000`
(Phoenix on the host). To scrape Phoenix running elsewhere, edit the target in
`prometheus.yml`.

### Wiring into an existing Prometheus instead

If you already run Prometheus, skip the compose stack and paste the job from
[`prometheus-scrape.yaml`](./prometheus-scrape.yaml) into your `prometheus.yml`
(set the `credentials` / `credentials_file`), then add Phoenix's Prometheus as a
Grafana data source — import [`provisioning/datasources/phoenix.yaml`](./provisioning/datasources/phoenix.yaml)
or point your existing Prometheus data source at this dashboard.

### Importing the dashboard manually

Without provisioning you can import the dashboard directly:
**Grafana → Dashboards → New → Import →** upload
[`dashboards/phoenix-overview.json`](./dashboards/phoenix-overview.json), then
pick your Prometheus data source when prompted (it maps to `$datasource`).

---

## Kubernetes / Helm

The Phoenix Helm chart can have Prometheus Operator scrape `/metrics` directly
via a `PodMonitor` — set `prometheus.podMonitor.enabled=true` and
`prometheus.apiKey=<phx_...>` in `values.yaml` (see
`charts/phoenix/templates/podmonitor.yaml`). Once Prometheus is scraping
Phoenix, add it as a Grafana data source and import
`dashboards/phoenix-overview.json` exactly as above — the dashboard is
deployment-agnostic.

---

## Validation

All YAML and the dashboard JSON in this directory parse cleanly, and every
PromQL expression references a metric/label that genuinely exists in
`internal/adapters/metrics/prometheus.go`. If you have `promtool`, you can
additionally check the scrape config with
`promtool check config prometheus.yml`.
