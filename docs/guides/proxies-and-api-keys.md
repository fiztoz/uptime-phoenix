# Proxies & API Keys

Both features are **admin-only** and live under **Settings** in the Phoenix UI.

---

## Proxies

### What It Is

A proxy lets you route a monitor's outbound HTTP/TCP/SOCKS check traffic through an intermediary server instead of connecting directly from the Phoenix pod. This is useful when:

- The target sits behind a firewall or VPN that only the proxy can reach.
- You need to test how a service looks from a specific geographic location or network.
- Corporate policy mandates all outbound traffic go through a forward proxy.
- You want to mask the origin IP of health-check requests.

Phoenix supports three protocols: **HTTP**, **HTTPS**, and **SOCKS5**. (SOCKS4 is intentionally excluded — Go's `x/net/proxy` package only implements a SOCKS5 dialer.)

### How to Set Up

1. Go to **Settings → Proxies**.
2. Fill in the form:
   - **Protocol** — `http`, `https`, or `socks5`.
   - **Host** — the proxy hostname or IP (e.g. `proxy.corp.example.com`).
   - **Port** — the proxy port (1–65535).
   - **Requires auth** — tick if the proxy needs a username/password.
   - **Active** — untick to temporarily disable the proxy without deleting it.
   - **Default** — tick to make this the default proxy for new monitors (at most one default per admin).
3. Click **Add proxy**.

You can edit or delete a proxy from the list at any time. Deleting a proxy that a monitor is currently using causes that monitor's checks to fall back to a direct connection on the next check tick.

### How to Assign to a Monitor

When creating or editing a monitor, the **Proxy** dropdown in the monitor form lists all active proxies. Select one to route that monitor's checks through it. Select "None" (or leave it blank) for direct connections.

Behind the scenes:
- The scheduler resolves the monitor's `proxy_id` to the actual proxy credentials before each check.
- The proxy configuration is cached for 30 seconds to avoid a DB query on every check tick.
- If the proxy is marked inactive or deleted, the check silently falls back to a direct connection.
- Only the HTTP checker currently supports proxies. TCP, ping, DNS, and other checker types connect directly regardless of proxy assignment.

### Default Proxy

Marking a proxy as **Default** does **not** auto-assign it to every monitor — it only pre-selects it in the monitor creation form. You must still explicitly assign it per monitor.

### Security Note

Proxy passwords are stored in plaintext in the database because the checker needs the raw credential to dial the proxy. They are **never** exposed via the API — the `GET /api/proxies` endpoint returns a `ProxyView` that omits the password field entirely. The backup export does include proxy passwords (to allow full restore), so treat backups as sensitive.

---

## API Keys

### What It Is

An API key is a long-lived credential that lets scripts, CI pipelines, and external tools talk to the Phoenix REST API without interactive browser login. API keys are scoped and can be revoked independently of the user account that created them.

API keys are **admin-only** — only an admin can create, list, or revoke them. A non-admin user cannot see the API Keys section at all.

### Scopes

Each key carries one or more **scopes** that control what it can do:

| Scope | What it unlocks |
|-------|-----------------|
| `read` | GET endpoints (list monitors, heartbeats, status pages, etc.) |
| `write` | Mutating endpoints (create/update/delete monitors, groups, users, notifications, maintenance, config-as-code apply) |
| `metrics` | The `/metrics` Prometheus endpoint |

A key with no explicit scopes defaults to `read`. You can combine scopes (e.g. `read` + `write` for full admin automation, or `metrics`-only for Prometheus scraping).

### How to Create

1. Go to **Settings → API Keys**.
2. Fill in the form:
   - **Name** — a descriptive label (e.g. `CI pipeline`, `Prometheus scraper`).
   - **Expires** — optional RFC 3339 date/time. Leave blank for no expiry.
   - **Scopes** — tick one or more (`read`, `write`, `metrics`).
3. Click **Create key**.
4. **Copy the key immediately.** The plaintext key (`phx_...`) is shown once and then gone forever — only the SHA-256 hash is stored.

### How to Use

Pass the key in one of these ways (all equivalent):

```
# Authorization header (preferred)
curl -H "Authorization: ApiKey phx_..." https://phoenix.example.com/api/monitors

# X-API-Key header
curl -H "X-API-Key: phx_..." https://phoenix.example.com/api/monitors

# Basic Auth (username = key, password ignored)
curl -u "phx_..." https://phoenix.example.com/api/monitors
```

The key authenticates as the user who created it. If that user is an admin, the key carries admin authority for `write`-scoped endpoints.

### What API Keys Can Do

| Endpoint group | Auth method | Required scope |
|---|---|---|
| `/api/monitors`, `/api/status-pages`, etc. | Session JWT **or** API key | `read` (GET) / `write` (POST/PUT/DELETE) |
| `/api/users` (admin user management) | Session JWT **or** API key | `write` + admin flag |
| `/api/config` (config-as-code) | Session JWT **or** API key | `write` + admin flag |
| `/api/backup/export`, `/api/backup/import` | Session JWT only | admin flag (API keys not accepted) |
| `/metrics` (Prometheus) | API key only | any active key (no scope check) |

### Example: Prometheus Scrape Config

```yaml
# prometheus.yml
scrape_configs:
  - job_name: phoenix
    metrics_path: /metrics
    authorization:
      credentials: phx_abc123...  # a metrics-scoped API key
    static_configs:
      - targets: ['phoenix.example.com:443']
    scheme: https
```

### Example: CI Pipeline (Config-as-Code)

```bash
# Export current config
curl -s -H "Authorization: ApiKey phx_..." \
  https://phoenix.example.com/api/config/export > config.yaml

# Validate a proposed config
curl -s -X POST -H "Authorization: ApiKey phx_..." \
  -H "Content-Type: application/json" \
  -d @config.json \
  https://phoenix.example.com/api/config/validate

# Apply (idempotent)
curl -s -X POST -H "Authorization: ApiKey phx_..." \
  -H "Content-Type: application/json" \
  -d @config.json \
  https://phoenix.example.com/api/config/apply
```

### Revoking a Key

Click **Revoke** next to any active key in the list. This is irreversible — any integration still using that key starts failing immediately. Issue a new key and update your callers.

### Expiry

A key with an `expires_at` date stops working after that timestamp. Expired keys are rejected silently (the middleware checks `APIKeyExpired` on every request). You can set or change expiry by creating a new key — there is no "edit key" endpoint.

### Security Notes

- Keys are `phx_`-prefixed, 256-bit random, base64url-encoded.
- Only the SHA-256 hash is stored — the plaintext is irretrievable after creation.
- Keys inherit the creating user's authority. If the user is deleted, their keys become orphaned (the hash lookup returns nothing, so they stop working).
- The `/api/backup/export` endpoint deliberately does **not** accept API keys — it requires a session JWT because the export contains secrets.
