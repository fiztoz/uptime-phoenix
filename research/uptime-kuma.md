# Uptime Kuma — Research Report

> Researched: 2026-06-22 · Repository: [louislam/uptime-kuma](https://github.com/louislam/uptime-kuma)
> Focus: tech stack, architecture, design system, and notable engineering patterns.

---

## 1. TL;DR

Uptime Kuma is a self-hosted, "fancy" monitoring tool by Louis Lam — the spiritual successor to the abandoned `statping` project. It is a single Node.js process that serves both a REST/Express layer and a Socket.IO realtime channel to a Vue 3 SPA. The default store is SQLite, but a thin **redbean-node** ORM abstraction supports swapping in MySQL/MariaDB, PostgreSQL, MSSQL, and even Oracle. The frontend is plain Vue 3 + Bootstrap 5 with Vite — no Pinia/Vuex, no TypeScript, no design-system library. The whole codebase intentionally stays small and pragmatic: a single `server/` folder, a single `src/` folder, ~90+ notification integrations, and 13+ monitor types, all reachable as one Docker image.

---

## 2. Quick Facts

| | |
|---|---|
| **Repo** | <https://github.com/louislam/uptime-kuma> |
| **License** | MIT |
| **Version (latest at time of research)** | 2.4.0 |
| **Author / maintainer** | Louis Lam (`louislam`) |
| **Node requirement** | `>= 20.4.0` |
| **Frontend** | Vue 3.5 + Vue Router 4 + Vue I18n 11 |
| **Build tool** | Vite 5 (`config/vite.config.js`) |
| **Styling** | Bootstrap 5.1 + custom SCSS (`src/assets/vars.scss`) |
| **Charts** | Chart.js 4 + `vue-chartjs` 5 + `chartjs-adapter-dayjs-4` |
| **Realtime** | Socket.IO 4 (server + client) |
| **Backend HTTP** | Express 4 + `express-static-gzip` |
| **ORM** | `redbean-node` (a Node port of RedBeanPHP) |
| **DB engines supported** | SQLite (default) · MySQL · MariaDB · PostgreSQL · MSSQL · Oracle |
| **Default port** | `3001` |
| **PM2/Docker/Native** | all first-class |
| **Docker pulls** | crossed 10M+ by Dec 2021; trend is one of GitHub's most-starred self-hosted tools |

### Project Origin (from the README)
Louis Lam wanted a "self-hosted monitoring tool like Uptime Robot". The closest contender was `statping`, but it was unstable and unmaintained. He also wanted to:
- Learn **Vue 3 + Vite**
- Show the power of **Bootstrap 5**
- Try **WebSocket with a SPA instead of REST** (this is a foundational design decision — see §5)
- Ship his first Docker image to Docker Hub

This origin story is important: it explains why the stack is "deliberately small". The architecture optimizes for a single developer shipping a single useful binary, not for hyperscale.

---

## 3. High-Level Architecture

```
                       ┌─────────────────────────────────────────────┐
                       │          Node.js process (server.js)        │
                       │                                             │
   Browser (SPA)       │   ┌────────────┐     ┌──────────────────┐   │
   Vue 3 + Bootstrap   │   │  Express   │     │    Socket.IO     │   │
       ▲               │   │  HTTP API  │     │   realtime bus   │   │
       │  REST          │   │ (auth,     │     │  (state + push)  │   │
       │  (init only)   │   │  status,   │     │                  │   │
       │                │   │  upload)   │     │                  │   │
       └────────────────┤   └─────┬──────┘     └────────┬─────────┘   │
                        │         │                     │             │
                        │         ▼                     ▼             │
                        │   ┌──────────────────────────────────┐     │
                        │   │   UptimeKumaServer core          │     │
                        │   │   ├─ Monitor scheduler (per-type)│     │
                        │   │   ├─ Notification dispatcher     │     │
                        │   │   ├─ Socket handlers (CRUD/auth) │     │
                        │   │   └─ Background jobs (cleanup)   │     │
                        │   └──────┬──────────────────────┬────┘     │
                        │          │                      │          │
                        │          ▼                      ▼          │
                        │   ┌────────────┐        ┌────────────┐    │
                        │   │ redbean-node│        │ monitor-   │    │
                        │   │   ORM       │        │ types/     │    │
                        │   └─────┬──────┘        │ plugins    │    │
                        │         │               └────────────┘    │
                        │         ▼                                  │
                        │   ┌────────────┐        ┌────────────┐    │
                        │   │  SQLite /  │        │ notif-     │    │
                        │   │  MySQL /   │        │ providers/ │    │
                        │   │  Postgres  │        │ (90+ integ)│    │
                        │   └────────────┘        └────────────┘    │
                        │                                             │
                       │   ┌────────────┐  ┌────────────┐            │
                       │   │  /metrics  │  │ /status/:  │            │
                       │   │ Prom API   │  │ slug (pub) │            │
                       │   └────────────┘  └────────────┘            │
                       └─────────────────────────────────────────────┘
                                        ▲
                                        │   (push from external networks)
                                  Push monitors
                                  (inbound HTTP)
```

### Key architectural properties

1. **One process, one port.** HTTP and WS share the same Express server; Socket.IO attaches to the underlying HTTP server. This is what makes the Docker story work (a single `EXPOSE 3001`).
2. **Real-time-first UI.** The frontend's *only* stable source of truth is Socket.IO events. REST is used for the bootstrap bundle, status-page rendering, and a few small read endpoints (`/metrics`, `/upload/*`). All monitor list edits, heartbeats, and notifications are pushed via WS — which is why the UI feels "fancy, reactive, fast" without any client-side store.
3. **Plugin-style monitor types and notifications.** New check types and notification integrations are added by dropping a single file into a folder and registering it (see `server/monitor-types/`, `server/notification-providers/`, mirrored by `src/components/notifications/` on the frontend).
4. **Database is hot-pluggable.** `redbean-node` exposes a stable `R.store()`, `R.load()`, `R.find()` API; the underlying driver is selected at startup. SQLite is the only engine that doesn't need external infrastructure, which is why it's the default for self-hosting.
5. **No client-side state management.** No Pinia/Vuex. A handful of mixins (`socket`, `theme`, `lang`, `datetime`, `mobile`, `public`) hold cross-cutting UI state, and component-local `ref`/`reactive` hold everything else. The server is the source of truth.

---

## 4. Tech Stack — Detailed

### 4.1 Backend (Node.js)

**Core**
- `express` ~4.22 — HTTP framework
- `socket.io` ~4.8 — realtime transport
- `socket.io-client` ~4.8 — used in tests + push-monitor helpers
- `redbean-node` ~0.3 — ORM (Node port of RedBeanPHP)
- `knex` ~3.1 — used as a query builder alongside redbean for migrations / complex SQL
- `@louislam/sqlite3` 15.1.6 — **forked** sqlite3 build (custom for this project, e.g. for cross-compile support)
- `pg`, `mysql2`, `mssql`, `oracledb` — drivers for the optional external DB engines
- `dotenv` — env loading
- `compression`, `express-static-gzip` — response compression + brotli
- `http-graceful-shutdown` — clean shutdown of long-running monitors

**Auth & security**
- `jsonwebtoken` ~9.0 — JWT issuance
- `bcryptjs` — password hashing
- `notp` — TOTP for 2FA
- `check-password-strength` — UI-side strength meter
- `password-hash` — additional hashing
- `openid-client` — OIDC SSO
- `helmet`-style protections are applied per-route in the routers

**Monitoring primitives** (one library per protocol family)
- `@louislam/ping` — custom ICMP ping fork (`0.4.4-mod.1`)
- `tcp-ping` — TCP-level "ping"
- `ws` ~8.19 — outbound WebSocket probes + WebSocket Upgrade monitor
- `dns2` — async DNS resolution
- `mqtt` ~4.3 — MQTT broker probe
- `net-snmp` — SNMP probes
- `radius` + `node-radius-utils` — RADIUS auth probes
- `mongodb` — MongoDB connect probes
- `kafkajs` — Kafka producer/consumer probe
- `mssql` — also reused for the MSSQL DB engine
- `@grpc/grpc-js` — gRPC health checks
- `node-cloudflared-tunnel` — embedded Cloudflare tunnel for the "no-port-forward" setup
- `node-ssh` — SSH-backed Docker / system-service monitoring
- `http-proxy-agent`, `https-proxy-agent`, `socks-proxy-agent` — proxy support
- `badge-maker` — SVG status badge generator
- `prom-client` + `prometheus-api-metrics` — `/metrics` endpoint

**Notifications (90+ providers)** — each is a one-file class under `server/notification-providers/`. Examples: Telegram, Discord, Slack, Pushover, Email/SMTP (`nodemailer`), Webhook, Signal, MS Teams, Mattermost, LINE, Pushbullet, PagerDuty, OpsGenie, Alerta, Gotify, Ntfy, N8N, Zabbix, etc. Frontend mirrors each provider with a Vue config component under `src/components/notifications/`.

**Misc utilities**
- `dayjs` + custom timezone plugin — date math everywhere
- `croner` ~8.1 — cron scheduling for background jobs
- `limiter` — rate-limiting outgoing notifications
- `chroma-js` — color manipulation for status colors
- `liquidjs` — templating for status-page custom messages
- `marked` — Markdown rendering on status pages
- `cheerio` — server-side HTML for the published status page
- `prismjs` — syntax highlighting on status pages
- `dompurify` — XSS sanitization of user-supplied HTML
- `qrcode` — 2FA setup QR
- `nanoid` — short ID generation
- `web-push` — browser push notifications
- `tldts` — public-suffix-aware domain parsing
- `tough-cookie` — cookie jar for HTTP keyword/JSON monitors
- `country-flag-emoji-polyfill` — country flag rendering
- `compare-versions` — version checking on the auto-updater
- `redbean-node` — see ORM section
- `nostr-tools` — NIP-17 Nostr DMs as a notification target

### 4.2 Frontend (Vue 3 SPA)

**Core**
- `vue` ~3.5 — composition API + options API mixed
- `vue-router` — client-side routing
- `vue-i18n` ~11.2 — i18n (weblate-driven; see §9)
- `vue-toastification` — toast notifications
- `vue-confirm-dialog` — confirm modals
- `vue-contenteditable` — inline-edit support
- `v-pagination-3` — pagination
- `@vuepic/vue-datepicker` ~3.4 — date/time pickers

**State / data**
- `socket.io-client` — sole persistent connection; no REST polling
- `mitt` ~3 — tiny event bus used inside the app
- `dayjs` + `dayjs/plugin/utc`, custom `timezone` plugin — date formatting

**UI / visualization**
- `bootstrap` 5.1.3 — CSS framework
- `@popperjs/core` — for Bootstrap dropdowns/tooltips/popovers
- `chart.js` ~4.2 + `vue-chartjs` ~5.2 — ping/uptime charts
- `chartjs-adapter-dayjs-4` — Chart.js time-axis adapter
- `@fortawesome/fontawesome-svg-core` + `@fortawesome/free-solid-svg-icons` + `@fortawesome/free-regular-svg-icons` + `@fortawesome/vue-fontawesome` ~3 — icon system
- `favico.js` — dynamic favicon badge (red dot when something is down)
- `sass` ~1.42 — SCSS compilation

**Build / dev**
- `vite` ~5.4 — dev server + production bundler
- `@vitejs/plugin-vue` ~5.0
- `vite-plugin-compression` — pre-compresses dist for the static-gzip middleware
- `rollup-plugin-visualizer` — bundle analysis
- `terser` — minification
- `concurrently` + `cross-env` + `wait-on` — orchestrate `vite` + `node server/server.js` in dev
- `core-js` — polyfills
- `eslint` + `eslint-plugin-vue` + `eslint-plugin-vue-scoped-css` + `eslint-plugin-jsdoc` + `eslint-config-prettier`
- `stylelint` + `stylelint-config-standard` + `stylelint-config-prettier` + `postcss-html` + `postcss-rtlcss` + `postcss-scss` — SCSS linting + RTL pipeline
- `prettier` ~3.8 — formatter
- `typescript` ~4.4 — type checker (JSDoc-flavored; the project itself is plain JS, TS is just for tooling)

**Tests**
- `playwright` / `@playwright/test` ~1.39 — end-to-end browser tests
- `testcontainers` + `@testcontainers/postgresql|mysql|mariadb|mssqlserver|oraclefree|rabbitmq|hivemq` — spin up real DBs/MQs in tests
- `test` ~3.3 — `node --test` runner
- `promisify-child-process` — promise-wrap shellouts in tests

### 4.3 Tooling outside Node

- `extra/healthcheck.go` — a tiny Go binary built per-arch (including `armv7`) and shipped in the Docker image, used by Docker `HEALTHCHECK`.
- `extra/release/{final,beta,nightly}.mjs` — release pipeline scripts
- Docker + `docker-compose` — primary distribution

---

## 5. Real-Time Architecture — Why It Matters

This is the project's most interesting architectural choice, and it's worth understanding in detail.

### 5.1 The "WebSocket-SPA" pattern

Uptime Kuma explicitly inverts the usual REST-first SPA model: **the only persistent API is a Socket.IO connection**. REST exists, but it's a fallback / static-asset channel.

The bootstrap flow is:

1. Browser hits `/` → Express serves the prebuilt `dist/index.html` (or the SPA shell, identical for all routes, courtesy of the catch-all `app.get("*", ...)`).
2. The Vue SPA mounts, and the very first thing `src/mixins/socket.js` does is `io({ url, transports: ["websocket"] })`.
3. On `connection`, the server emits a `setup` event if the user hasn't been initialized, or emits the full state for the authenticated socket (monitors, status pages, notifications, etc.).
4. After that, every CRUD operation, every heartbeat, every state change is a push from the server via Socket.IO events back to one or more sockets.

### 5.2 Server-side organization

The server's main loop is in `server/server.js`, but the contributing guide says:

> Most server logic is encapsulated in the `socket.io` handlers. `express.js` is also used to serve [static assets and a few routes].

So the request flow is:

```
HTTP  /, /dashboard, /status/...     →  Express static + catch-all → SPA
HTTP  /metrics                        →  Basic-auth + prom-client exporter
HTTP  /upload/..., /api/...           →  routers/  (auth, attachments, etc.)
WS    on("connection")                →  socket-handlers/  (CRUD, login, heartbeats)
```

The `socket-handlers/` directory mirrors the resource boundaries (monitor, notification, status-page, settings, login, etc.) and each file exports handlers that the `io.on("connection", ...)` router in `server.js` wires up.

### 5.3 Why this is fast

- **No polling.** A monitor flips state and every connected client gets a push within a few ms.
- **No optimistic-update logic to debug.** The server is the source of truth; the client is a renderer.
- **No client cache invalidation.** When the schema changes server-side, the next `info` event rehydrates the UI.
- **Single transport.** Easier to reason about than REST + WebSocket + Server-Sent Events.

The tradeoff is that **the SPA cannot work without the WS connection**, and the project ships a fallback: if `io` fails to connect, the app surfaces a clear "not connected" state rather than pretending to work.

---

## 6. Source Code Structure

The repo is intentionally shallow. From `CONTRIBUTING.md` and the `src/`, `server/` trees:

### 6.1 Frontend — `src/`

```
src/
├── App.vue                      # root component
├── main.js                      # Vue app bootstrap (createApp + plugins)
├── router.js                    # vue-router routes
├── i18n.js                      # vue-i18n init + locale loaders
├── icon.js                      # FontAwesome registry
├── util-frontend.js             # browser-side helpers
├── util.ts                      # tiny TS helper file
├── assets/
│   ├── app.scss                 # main SCSS entry (imports vars, bootstrap, custom)
│   ├── vars.scss                # design tokens (see §8)
│   ├── vue-datepicker.scss      # datepicker overrides
│   └── icons/                   # SVG icons
├── components/                  # reusable Vue components
│   ├── notifications/           # one config form per notif provider (~90+)
│   ├── settings/                # settings UI
│   ├── monitors/                # monitor-type-specific forms
│   └── ...
├── pages/                       # route-level components
│   ├── Dashboard.vue
│   ├── List.vue
│   ├── Settings.vue
│   ├── StatusPage.vue           # public status page
│   └── ...
├── layouts/                     # layout shells (e.g. AuthLayout.vue)
├── mixins/                      # shared logic
│   ├── socket.js                # the global socket connection
│   ├── theme.js                 # light/dark toggle
│   ├── lang.js                  # locale switching
│   ├── datetime.js              # date formatting
│   ├── mobile.js                # mobile breakpoint detection
│   └── public.js                # public status page view mode
├── modules/dayjs/               # bundled dayjs with custom timezone plugin
└── lang/                        # i18n locale files (see §9)
```

### 6.2 Backend — `server/`

```
server/
├── server.js                    # entry point (Express + Socket.IO + jobs)
├── jobs.js                      # periodic background jobs
├── notification.js              # the notification dispatcher core
├── database.js                  # redbean-node bootstrap
├── uptime-kuma-server.js        # UptimeKumaServer class (was the "new" core)
├── image-data-uri.js            # helper for image attach
├── analytics/                   # uptime aggregation
├── jobs/                        # cron-style jobs (cleanup, ssl expiry, etc.)
├── model/                       # ORM model classes (one per table)
├── modules/                     # local forks / patches of 3rd-party modules
├── monitor-conditions/          # extra conditions for evaluating a heartbeat
├── monitor-types/               # one file per monitor type
│   ├── http.js                  # HTTP(s)
│   ├── tcp.js                   # TCP socket
│   ├── ping.js                  # ICMP ping
│   ├── dns.js                   # DNS record
│   ├── websocket-upgrade.js     # WS upgrade probe
│   ├── snmp.js                  # SNMP
│   ├── mqtt.js                  # MQTT broker
│   ├── mongodb.js               # MongoDB
│   ├── kafka.js                 # Kafka
│   ├── radius.js                # RADIUS
│   ├── gamedig.js               # Steam / game servers
│   ├── docker.js                # Docker container
│   ├── system-service.js        # local systemd / service
│   ├── tailscale-ping.js        # Tailscale ICMP
│   └── ...                      # many more
├── notification-providers/      # one file per notif integration (~90+)
│   ├── telegram.js
│   ├── discord.js
│   ├── slack.js
│   ├── smtp.js                  # email
│   ├── webhook.js
│   ├── signal.js
│   ├── ntfy.js
│   ├── pushover.js
│   ├── teams.js
│   ├── mattermost.js
│   ├── pagerduty.js
│   ├── opsgenie.js
│   ├── nostr.js
│   └── ...
├── routers/                     # Express routers (auth, status-page, upload, etc.)
├── socket-handlers/             # Socket.IO handler modules
└── prometheus/                  # /metrics exporter
```

### 6.3 The "one-file plugin" pattern

Both `monitor-types/` and `notification-providers/` follow the same convention: drop a file, export a class, and a single registry near the entry point picks it up. This is what allows the project to scale to 90+ notification providers without `if/else` chains or DI containers.

For monitor types, the class must implement a `check(...)` method (and a few metadata fields). For notifications, the class must implement `send(notification, msg, monitorJSON, heartbeatJSON)`. The `notification.js` module holds the loop that calls `send` on every provider, with `limiter` enforcing rate caps and `mitt` mediating internal events.

---

## 7. Monitor Types

The README lists the headline set, but the source tree reveals the full inventory (selected):

| Type | Source file | Notes |
|---|---|---|
| HTTP(s) | `http.js` | classic GET/POST/PUT/DELETE with headers/body/auth, TLS cert info, expected status code, keyword/JSON-query variants |
| HTTP(s) Keyword | same family | asserts a substring in the response body |
| HTTP(s) Json Query | same family | JSONPath assertion on the response |
| TCP | `tcp.js` | raw TCP connect, optional send/receive |
| Ping | `ping.js` | ICMP via the `@louislam/ping` fork |
| DNS | `dns.js` | resolve a record, optionally assert value |
| WebSocket | `websocket-upgrade.js` | upgrade handshake + optional message roundtrip |
| Push | special | inbound HTTP — an external agent POSTs heartbeats to Uptime Kuma |
| Steam Game Server | (uses `gamedig`) | query a game server |
| Docker Container | `docker.js` (via `node-ssh` or local socket) | CPU/mem/uptime of a container |
| MongoDB, MySQL, MSSQL, PostgreSQL, Redis | respective drivers | connect + ping |
| MQTT | `mqtt.js` | subscribe + assert message |
| SNMP | `snmp.js` | OID walk/GET |
| Radius | `radius.js` | auth probe |
| gRPC | `grpc.js` | health-check protocol |
| Kafka | `kafka.js` | producer/consumer |
| Systemd service | `system-service.js` | `systemctl is-active` |
| Tailscale Ping | `tailscale-ping.js` | uses the `tailscale` CLI |

Heartbeats are persisted to a single `heartbeat` table; the analytics module downsamples them into `1m` / `1h` / `1d` aggregate tables for fast chart rendering on the status page.

---

## 8. Design System

Uptime Kuma deliberately does **not** ship a Figma-style design system or a component library. The whole UI is **Bootstrap 5 with a small layer of custom SCSS variables** for the brand palette and a handful of dark-mode overrides.

### 8.1 Token file — `src/assets/vars.scss`

```scss
$primary:        #5cdd8b;   // signature green
$danger:         #dc3545;   // Bootstrap red
$warning:        #f8a306;   // amber
$maintenance:    #1747f5;   // blue — used for maintenance windows
$link-color:     #111;
$border-radius:  50rem;     // pill-shaped controls (very deliberate UX)
$secondary-text: #aaa;

$highlight:        #7ce8a4; // success background tint
$highlight-white:  #e7faec; // light surface for callouts

// Dark theme
$dark-font-color:  #b1b8c0;
$dark-font-color2: #020b05;
$dark-bg:          #0d1117;  // GitHub-dark-like
$dark-bg2:         #070a10;
$dark-border-color:#1d2634;
$dark-header-bg:   #161b22;

// Motion
$easing-in:  cubic-bezier(0.54, 0.78, 0.5...);   // custom easings
```

The variables are then imported by `src/assets/app.scss` which:
1. Overrides Bootstrap 5 defaults before `@import "bootstrap"`.
2. Adds custom component classes for status pills, monitor cards, status-page bars, etc.
3. Provides RTL overrides via `postcss-rtlcss` for languages like Arabic/Persian/Hebrew.

### 8.2 Design language observations

- **Pill-shaped controls** (`border-radius: 50rem`) — the entire UI leans on rounded pills, not sharp rectangles.
- **Color-as-state.** Green = up, red = down, yellow = pending, blue = maintenance. State is *the* primary visual channel.
- **Dense tables with generous status badges.** The dashboard is a single long list of monitors, each with a status pill, latency sparkline, and uptime %.
- **Dark mode is first-class.** Toggled by a `data-theme` attribute on `<html>`; the custom dark variables flip backgrounds, borders, and font colors.
- **Reactive, "live" feel.** Because updates arrive via Socket.IO, the UI never has to refresh — it just animates between states with CSS transitions on the custom easings.
- **i18n baked in from day one.** Every string lives in `src/lang/*.json`, contributing guide explicitly references Weblate.
- **No mascot / no marketing visuals.** The logo is a small SVG (`public/icon.svg`) used in the header and as the favicon. The product does the talking.

### 8.3 Why so minimal?

It's a deliberate trade-off. A design system would let the UI scale to many more screens, but Uptime Kuma has a small surface area (dashboard, settings, status page, a few modals). Bootstrap 5 covers that surface adequately and the `vars.scss` override layer means a single change to `$primary` would re-skin the entire app — which is the *only* level of theming the project needs.

---

## 9. i18n

- Powered by `vue-i18n` ~11.2.
- Locales live in `src/lang/*.json`.
- Translation is done on a self-hosted **Weblate** instance (link in the contributing guide).
- The README, repo topics, and the project wiki are all translated by the community.

This is one of the project's strongest adoption levers — it's why a self-hosted tool can grow to 50+ community translations without the maintainer personally maintaining them.

---

## 10. Data Layer

### 10.1 ORM — redbean-node

The choice of [`redbean-node`](https://github.com/saltec/redbean-node) is unusual and worth noting. RedBeanPHP was popular in the PHP world for "convention over configuration" — you don't write migrations or define schemas, you just call `R.store(bean)` and the table is auto-created and altered as needed.

Uptime Kuma leans on this for the bulk of its persistence (monitors, heartbeats, status pages, users). For more complex queries (e.g. the analytics rollups), it falls back to `knex` directly.

### 10.2 Schema

The schema is auto-managed. The high-level entities are:

- `user` — login credentials, 2FA, JWT secret, role
- `monitor` — the monitor definition
- `heartbeat` — every check result, with latency, status, msg
- `heartbeat_1m`, `heartbeat_1h`, `heartbeat_1d` — downsampled rollups for fast charting
- `notification` — notification configuration (one row per provider config)
- `status_page` + `status_page_cname` + `status_page_monitor` — public status pages with custom domains
- `tag` + `monitor_tag` — tags for grouping
- `proxy` — outbound HTTP/SOCKS proxies
- `maintenance` — scheduled maintenance windows
- `api_key` — Prometheus API keys
- `incident` — incident tracking on the status page
- `docker_host` — Docker daemon connection details
- `tls_info` — cached cert info per monitor
- `setting` — key/value app settings

### 10.3 Realtime persistence model

When a monitor runs, the cycle is:

```
schedule.tick → monitor-type.check()
   ├─ on result: write heartbeat row → emit "heartbeat" socket event
   ├─ on status change: write notification rows → call each notif-provider.send()
   └─ periodically: aggregate into heartbeat_1m/_1h/_1d
```

Status pages are pre-rendered HTML by Express routes, but the dashboard is **only** Socket.IO.

---

## 11. Deployment & Operations

### 11.1 Distribution
- **Docker image** — primary, multi-arch (`linux/amd64`, `linux/arm64`, `linux/arm/v7`).
- **Native Node** — `git clone && npm install && node server/server.js`.
- **pm2** — recommended for native installs.
- **AUR package** — for Arch.
- **Cloudflare Tunnel** — `node-cloudflared-tunnel` is built in for users who can't port-forward.

### 11.2 Notable runtime concerns
- `data_dir` is configurable (env: `DATA_DIR`); the SQLite file, uploaded attachments, and cert cache all live there.
- The Go `healthcheck` binary is compiled per-arch and used by Docker `HEALTHCHECK` to verify the app is alive (the Node process can be wedged in a way that doesn't 500, so a separate probe is needed).
- `http-graceful-shutdown` ensures in-flight monitor checks finish before the process exits — important because a kill -9 would leave zombie `ping`/`tcp` children.

### 11.3 Observability
- `/metrics` — Prometheus exporter, Basic-auth using the first user's password.
- Console logs — go through a custom log utility that includes the calling module and timestamp.
- No external telemetry. The project is self-hosted by definition and doesn't phone home.

---

## 12. Engineering Patterns Worth Borrowing

These are the patterns that make Uptime Kuma a great reference project, regardless of whether you build a monitoring tool.

### 12.1 Real-time-first SPA
If your UI is heavily stateful and changes come from the server anyway, skip REST polling. One persistent WebSocket + a single state-rehydration event is far simpler than `useQuery` + invalidation strategies. The cost is you can't `curl` it, but for internal tools that price is often fine.

### 12.2 The "one-file plugin" convention
For every monitor type and notification provider, **the entire integration is a single file** with a single class implementing a small interface. No config files, no DI, no decorators. This is what makes it tractable to support 90+ notification providers as a single-maintainer project.

### 12.3 ORM with auto-migration
RedBean-style "just call `store()`" removes 90% of the schema-maintenance pain. The cost is you give up strict schema control — acceptable for an app where the schema is internal and the only consumer is the app itself.

### 12.4 Database hot-pluggability
By keeping all DB access behind `R.store()` / `R.load()` and writing nothing dialect-specific, the same binary supports SQLite (zero-config) and Postgres/MySQL/MSSQL/Oracle (production-grade). The README emphasizes this is a key feature.

### 12.5 Community-driven i18n via Weblate
Don't hand-roll translation infrastructure. Wire `vue-i18n` to a Weblate instance and let the community translate for free. The project's lang file directory has 50+ locales.

### 12.6 The "deliberately small" design system
A 30-line `vars.scss` that overrides Bootstrap 5's defaults is enough for most B2B internal tools. Skip the Storybook + tokens pipeline unless the UI actually needs it.

### 12.7 Custom Go healthcheck for Docker
If your Node process can wedge in a way that HTTP doesn't catch, ship a tiny `healthcheck.go` per-arch alongside the binary. It's 50 lines of Go and gives you a reliable Docker `HEALTHCHECK`.

### 12.8 Graceful shutdown for long-running work
`http-graceful-shutdown` plus a small wrapper that stops accepting new checks and waits for in-flight ones to finish is the difference between a "clean restart" and "lose 5 minutes of heartbeats on every deploy".

---

## 13. Things Uptime Kuma Doesn't Do

For honesty:

- **No multi-tenant** — single user, single installation, single tenant. If you need SaaS multi-tenancy, this is the wrong base.
- **No distributed / sharded mode** — single process, single DB. The `node-cloudflared-tunnel` is for remote workers, not for HA.
- **No client-side router-level data fetching** — the dashboard depends entirely on the socket. If the socket is down, the dashboard is dead.
- **No TypeScript** — the frontend is plain JS + JSDoc types where it matters. This is a deliberate "move fast" choice, and the codebase does feel harder to navigate as a result.
- **No formal design system** — extending the UI requires Bootstrap knowledge, not "design system knowledge".

---

## 14. Takeaways for a New Project

If you're building a similar self-hosted, web-based, long-running tool:

1. **Default to a real-time transport if the UI is mostly state-driven.** WebSocket-SPA is dramatically simpler than REST polling for this use case.
2. **Choose an ORM that minimizes schema maintenance.** redbean, Prisma with auto-migrate, or Drizzle with auto-migrations. Hand-rolled migrations are a tax you don't need at v0.
3. **Make the database engine swappable from day one.** It costs almost nothing up front and it pays for itself the moment someone wants Postgres.
4. **Use Bootstrap 5 + a small SCSS override layer** if you need a working UI in a week. Skip a design system until you actually have designers.
5. **Ship a Go healthcheck binary alongside Node** if your app's failure modes are subtle. Docker's built-in `curl localhost` is too coarse.
6. **Adopt Weblate for i18n from day one.** It's free for open source and unblocks a huge community contribution lever.
7. **Use a "one-file plugin" convention for integrations.** It scales way better than a DI container for integrations.

---

## 15. Sources

- Repo: <https://github.com/louislam/uptime-kuma>
- README: <https://raw.githubusercontent.com/louislam/uptime-kuma/master/README.md>
- `package.json`: <https://raw.githubusercontent.com/louislam/uptime-kuma/master/package.json>
- `server.js`: <https://raw.githubusercontent.com/louislam/uptime-kuma/master/server/server.js>
- `src/main.js`: <https://raw.githubusercontent.com/louislam/uptime-kuma/master/src/main.js>
- `CONTRIBUTING.md`: <https://raw.githubusercontent.com/louislam/uptime-kuma/master/CONTRIBUTING.md>
- `src/assets/vars.scss`: <https://raw.githubusercontent.com/louislam/uptime-kuma/master/src/assets/vars.scss>
- Wiki — Architecture: <https://github.com/louislam/uptime-kuma/wiki/Architecture>
- Wiki — Status Page: <https://github.com/louislam/uptime-kuma/wiki/Status-Page>
- Wiki — Development: <https://github.com/louislam/uptime-kuma/wiki/%E2%80%90-How-to-Develop>
- Source trees: `src/`, `server/`, `src/components/`, `server/monitor-types/`, `server/notification-providers/`

> All URLs verified during research on 2026-06-22. Where a URL returned 404 (e.g. `server/package.json`, `docker-compose.yml` at the root, `STRUCTURE.md`), it is because the project has a single root `package.json` and uses a different layout — the data was still reconstructable from the server tree and the contributing guide.
