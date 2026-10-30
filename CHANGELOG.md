# Changelog

Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Released versions are git tags (`vX.Y.Z`). New work lands under `[Unreleased]`
until the next tag. Historical notes below `[0.1.0]` were written before the
first tag and stay grouped by conventional-commit type.

The project's first ~4 weeks (2026-06-23 – 2026-06-30, pre conventional-commit discipline) are
summarized rather than itemized — see [Earlier foundation](#earlier-foundation-2026-06-23--2026-06-30)
at the bottom.

## [Unreleased]

### Added

- `GET`/`PUT /api/probes/:probe_id/watchdog` (admin session or admin
  write-scope API key) reads and replaces a probe's offline-alert watchdog
  settings, so probes enrolled through the API or UI can turn on probe-down
  alerts without `phoenix-probe-admin` (#120). The watchdog stays off until an
  explicit PUT; `expected_revision`, `enabled` and `notification_ids` are
  required, omitted timings take the same defaults as `phoenix-probe-admin
  watchdog` (90 s lost, 30 s recover, no resend, from one shared source), and
  enrollment never picks channels. Unknown fields and explicit nulls are
  rejected. A save is the desired setting only, not an
  applied receipt. Stale revisions and disabled probes answer 409, an unwired
  hub answers 501, and `PROBES_ENABLED=false` answers 503.

## [0.6.2] — 2026-10-28

Use 0.6.2 rather than 0.6.1. v0.6.2 is the first signed release whose arm64
images contain arm64 binaries.

### Fixed

- **Release arch check pulled platforms by the index digest (#111).**
  `scripts/release/verify-image-arch.sh` created a container for each platform
  from the image index digest with `--platform`. On Docker's classic image
  store, which GitHub runners use, the second platform failed with
  `cannot overwrite digest`. Each platform is now checked by its own manifest
  digest from the index, and a regression test that models the classic store
  runs in `make release-image-gate`.

### Known issues

- **v0.6.1 was published unsigned and has no GitHub Release.** Because of the
  release-check bug above, the v0.6.1 publish jobs stopped after pushing. The
  `uptime-phoenix`, `uptime-phoenix-api`, `uptime-phoenix-worker` and
  `uptime-phoenix-probe` 0.6.1 images have no cosign signature (only
  `uptime-phoenix-web` was signed), and no GitHub Release or binaries were
  created. The 0.6.1 images and chart do contain the correct architecture, but
  they cannot be verified with cosign. The v0.6.1 tag and images are left in
  place.

### Upgrade notes

- No schema migrations and no configuration changes compared with 0.6.1. The
  application is the same as 0.6.1. Deploy 0.6.2 (images and chart) instead of
  0.6.1, and replace any 0.5.1/0.6.0 images on arm64 hosts with 0.6.2.

## [0.6.1] — 2026-10-28

### Fixed

- **arm64 images now contain arm64 binaries (#108).** The `linux/arm64`
  variants of the published 0.5.1 and 0.6.0 images (`uptime-phoenix`,
  `uptime-phoenix-api`, `uptime-phoenix-worker`, and `uptime-phoenix-probe` in
  0.5.1) contained x86-64 binaries, so containers on Graviton/ARM hosts exited
  with `exec /uptime-phoenix: exec format error`. The Go builder stages of
  `Dockerfile` and `Dockerfile.split` gave the BuildKit platform args defaults
  (`ARG TARGETARCH=amd64`), which replaced the value derived from `--platform`.
  They are now bare `ARG TARGETOS` / `ARG TARGETARCH`. amd64 images are
  unchanged.

### Changed

- **Release-time ELF architecture verification (#108).** After each image push
  and before cosign signs it, the release workflow extracts the binary from
  every platform of the pushed digest and fails if its ELF machine does not
  match the platform. The release dry-run and CI now build exactly like
  publish (`--platform` only, no `TARGET*` build-arg overrides), CI builds and
  checks every Go image for amd64 and arm64, and `make release-image-gate`
  rejects platform ARG defaults statically.

### Security

- **go-jose bumped to v4.1.6 (#110).** `github.com/go-jose/go-jose/v4`
  v4.1.4 → v4.1.6 (GHSA-mgcc-cww5-fp2q); indirect via `coreos/go-oidc/v3`
  for OIDC login. govulncheck reported it unreachable.
- **osv-scanner exceptions refreshed (#110).** Added a dated ignore (until
  2026-12-15) for GHSA-gcqx-w48m-w2h4 in deepmerge 4.3.1, which has no fix and
  is used only at build time by `@sveltejs/vite-plugin-svelte`, not shipped.
  Removed three ignores that no longer match anything.

### Upgrade notes

- No schema migrations and no configuration changes. On arm64 hosts, replace
  0.5.1/0.6.0 images with 0.6.1; amd64 deployments can upgrade at leisure.

## [0.6.0] — 2026-10-26

### Added

- **Aggregate paging for multi-region probes (M7, default-disabled).** Desired
  `alert_delivery` (`regional`, `aggregate`, `both`) is stored on assignment
  sets and carried in probe config (#81). With `PROBES_AGGREGATE_PAGING_ENABLED`,
  a receipt-backed cutover moves every source through quiesce, apply, active,
  drain and resume before the hub pages from aggregate incidents: AGGREGATE
  suppresses regional availability alerts, BOTH keeps them. Provider sends are
  re-authorized against the database clock immediately before I/O and recorded
  in attempt journals (#84). Regional paging is unchanged while the flag is off.
- **Optional SSH probe provisioning (M8, admin-only, default-disabled).** With
  `PROBE_SSH_INSTALL_ENABLED`, an admin installs a regional probe on a Linux host
  over SSH with a pinned host key: read-only host preflight, a durable install
  journal of one-shot Ed25519-granted steps, activation (init, registration,
  service start, enrollment) and exact cleanup. Fixed 422 codes report every
  refusal; remote output and the enrollment token are never stored (#86).
  Accepted on AWS Ubuntu 24.04 arm64 and amd64.
- **Docker install path for M8.** `POST /api/probe-installs` with
  `mode: "docker"` and an image pin installs the probe as a container from the
  operator's OCI layout. The host helper loads only the pinned image and runs it
  as root with `CAP_NET_RAW` only, `no-new-privileges` and a read-only root
  (#99). See `docs/multi-region/M8_DOCKER_INSTALL.md`.
- **One-time SSH password host bootstrap for the M8 probe installer.** Admins can
  now prepare a probe host from the hub with an SSH password
  (`POST /api/probe-host-bootstraps`, and *Prepare host (SSH password)* on the
  Probes page) instead of the manual root setup. The hub:
  - checks the pinned host key before sending the password;
  - installs the pinned `phoenix-probe-admin` helper, its stage-authority public key
    and a `restrict` forced-command `authorized_keys` entry for the deployment key
    (atomically and idempotently; it never rewrites unrelated lines and refuses
    conflicts rather than overwriting them);
  - verifies the restricted key with the real install preflight.

  The password, also used for `sudo -S` when the user is not root, lives only for
  that request. It is never stored, logged or returned. The endpoint is admin-only
  and rate-limited to 5 attempts a minute. Installs still use the restricted key
  only. See `docs/multi-region/M8_HOST_BOOTSTRAP.md`.

### Security

- Monitors and notifications now reach only what their **owner** may reach
  (#88). Those owned by an admin are refused only the always-blocked floor:
  link-local (`169.254.0.0/16`, `fe80::/10`, including `169.254.169.254`),
  cloud metadata (`fd00:ec2::254`, `100.100.100.200`, Azure `168.63.129.16`),
  multicast and unspecified addresses, which nothing can allowlist. Those owned
  by a non-admin (or by a deleted user) are also refused loopback, RFC 1918,
  CGNAT and IPv6 ULA destinations unless an operator lists them in
  `OUTBOUND_ALLOWED_CIDRS` (Helm `outbound.allowedCIDRs`). Remote probes apply
  the floor only. The check runs on the resolved address at dial time, so
  redirects, DNS rebinding and IPv4-mapped/NAT64 forms are covered for HTTP(S),
  S3, WebSocket, TCP, gRPC, RabbitMQ, MQTT, DNS (the resolver dial), SNMP and
  ping checks, and for every HTTP and SMTP notifier. Through `HTTP(S)_PROXY` or
  a monitor proxy, the target is resolved and checked before the request is
  handed to the proxy. **Upgrade:** admin-only installs need no change; a
  startup warning counts non-admin-owned monitors whose targets are now
  refused. See RUNBOOK "Outbound guard".
- Admin-only now (#88): `docker` and `database` monitors (the database drivers'
  dialers are not guarded), assigning a monitor `proxy_id`,
  `save_body_on_error`/`save_body_on_success` (only on admin-owned monitors),
  and changing the destination of a notification an admin created. The monitor
  form hides these options from non-admins.
- For non-admin-owned monitors, JSON-assertion failures and OAuth2 token
  errors no longer echo response content; admin-owned monitors keep the
  truncated actual value (#88).
- The chart NetworkPolicy keeps in-cluster HTTP(S) targets reachable (a
  `namespaceSelector` peer) and excludes the metadata addresses from its
  `ipBlock` egress peers (#88).
- **Breaking:** `JWT_SECRET` is now required and must be at least 32 bytes;
  the published default secrets were removed from compose files, the Helm
  chart and docs (#87). This applies to **every** binary that loads the hub
  config, including the split `worker` and `phoenix-probe-admin`: the chart
  and both compose files now give the worker the API's key, but a
  hand-written worker manifest or compose file must add the same
  `JWT_SECRET` as the API or the worker exits at start. `make run`, `make dev`
  and `make dev-split*` use a per-checkout random key in
  `.secrets/dev_jwt_secret`.
- Monitor and notification secrets (webhook URLs, tokens, DSNs, auth
  headers, URL passwords, the push monitor's `hmac_secret`, and webhook
  headers saved as a JSON string) are write-only. API and WebSocket
  responses carry `__REDACTED__`; sending it back on update keeps the stored
  value. A push monitor's token is still shown to users who can edit that
  monitor (#89). A masked list entry that cannot be matched to its stored
  entry is refused with 400 instead of being restored by position.
- A monitor's displayed target (WebSocket cards, notification messages)
  never carries credentials: a database monitor shows `host:port/db`, not
  its DSN, and URLs lose their `user:password@` (#89).
- The webhook notifier now sends extra headers saved as a JSON string (the
  form's "Extra Headers (JSON)" field), which were silently ignored, and
  rejects headers that are not a JSON object of strings when saved.
- Sign-in is throttled: login, 2FA and passkey verification share a per-IP
  budget (10 failed attempts per minute); five failed password or TOTP
  attempts from one client IP back that IP off the account exponentially
  (30 s doubling to 15 min); fifty from any mix of IPs back off the account
  for everyone; a 2FA ticket is spent after five codes. Accounts are keyed by
  stored user ID, so collation variants of a username share one budget, and
  attempts are reserved before the password check so parallel requests
  cannot exceed it. See docs/RUNBOOK.md, "Sign-in lockout", for unlocking
  (#90).
- **Breaking (Helm, split web):** the web pod runs as uid 101 on a read-only
  root with `seccompProfile: RuntimeDefault`, listening on 8080 (the Service
  port stays 80). Images built from this tree use nginx-unprivileged. The
  chart also mounts `/run` and `/var/cache/nginx` as emptyDirs, so the
  already-published root-based `uptime-phoenix-web` image (0.5.1 and older)
  runs unchanged under the new security context; anything that reached the
  web pod's port 80 directly (custom NetworkPolicies, PodMonitors, sidecars)
  must use 8080. Every chart workload, including the backup, partition,
  rollup and migrate jobs, runs non-root under `seccompProfile:
  RuntimeDefault` (#91).
- **Breaking:** creating the first admin from the web UI now needs the one-time
  setup token that the server logs at startup while it has no users and no
  `BOOTSTRAP_USERNAME`/`BOOTSTRAP_PASSWORD`. The token is derived from
  `JWT_SECRET`, so every replica logs the same one; `JWT_SECRET` (now required, #87) keeps it
  unguessable.
  First-user creation is now atomic across replicas: concurrent sign-ups on
  an empty install create exactly one admin (#92).
- **Breaking:** `docker-compose.yml`, `docker-compose.split.yml` and the Helm
  chart now default to `PRODUCTION=true`, `LOG_LEVEL=info` and no bootstrap
  admin password. Production mode serves a strict Content-Security-Policy:
  `script-src` allows only the SPA's own files and the SHA-256 hashes of its
  inline bootstrap script, computed from the embedded build at startup. Use
  `docker-compose.dev.yml` for the old relaxed local defaults. The chart now
  refuses `bootstrap.enabled` without a password or existing Secret. The
  binary's own default stays `PRODUCTION=false` (#92). Upgrade notes:
  Helm releases that kept `bootstrap.enabled: true` after removing the
  password must set `bootstrap.enabled: false` (the chart now fails to render
  otherwise); production mode also turns CORS to deny-by-default, so browser
  code on another origin that calls the API must be listed in
  `CORS_ALLOW_ORIGINS` (same-origin UI, the split nginx proxy and badge
  images are unaffected).
- CI pins every action to a commit SHA, its token is read-only by default,
  and only the docker job gets `actions: write` (for the build cache).
  `make action-pins` and CI fail on any tag-only `uses:` (#92).
- Dependencies: echo 4.16.1, dompurify 3.4.17 and a refreshed `web/bun.lock`.
  CI runs osv-scanner over `go.mod` and `web/bun.lock`. The advisories that
  can't be fixed yet are recorded, with reasons and an expiry date, in
  `osv-scanner.toml` (#92).
- Dependabot version PRs wait out a 7-day cooldown (14 days for Go and npm
  majors). A gitleaks pre-commit hook (`.pre-commit-config.yaml`,
  allowlist in `.gitleaks.toml`) and `make secrets-scan` catch committed
  secrets (#92).
- The chart NetworkPolicy no longer opens 3000/80/8080 egress to every
  destination for the tunnel: in-release traffic is scoped to this
  release's pods, and cloudflared alone may reach Cloudflare's edge on 7844.
- Align active build, CI, release, Docker, E2E and offline verification pins to
  Go 1.26.10, fixing the reachable standard-library advisory GO-2026-6809
  (distrust compromised intermediate certificates in `crypto/x509`).
- Update Go modules, including `golang.org/x/net` v0.60.0, and align active build,
  CI, release, Docker, E2E and offline verification pins to Go 1.26.9. This fixes
  the reachable standard-library vulnerabilities that blocked dependency PRs;
  vulnerability scanning remains mandatory and unsuppressed.
- SvelteKit 3.1 and adapter-static 4 (#105) close two SvelteKit advisories
  (GHSA-27mw-82g8-phjj, GHSA-pfhg-q3qv-9q6r). Neither was reachable: the app
  uses no `setHeaders` and no `use:enhance`.

### Fixed

- Regional history keeps the newest rows when timestamps tie (#71).
- Edge maintenance observations are recorded (#72); deleting a monitor removes
  its partitioned heartbeat history (#73).
- Delayed outage summaries persist across edge recovery (#74), and a failed
  ingest health state holds until durable replay recovers (#75).
- Replay acquires observation-assignment and history-commit locks in a fixed
  order, removing MariaDB deadlocks (#77, #78).
- Status-page recovery filters before reading health (#79).
- `phoenix-probe-admin help` (and usage errors) no longer require
  `JWT_SECRET`; only the subcommands that open the hub database load its
  config. The release artifact check runs `help` on the built binary.

### Changed

- Go toolchain 1.27.3 across `go.mod`, CI, release and every Docker builder
  (#104); golangci-lint v2.15.0 and govulncheck v1.9.0 in CI (#104, #106).
- The frontend uses SvelteKit 3: project config moved from `svelte.config.js`
  into `vite.config.ts`, and `$lib` imports became the `#lib` subpath import
  (#105).
- Dependabot updates `web/` with the `bun` ecosystem; the `npm` entry failed on
  every run because it cannot update `bun.lock` (#105).
- Go module, GitHub Actions and frontend dependency updates (#82, #83, #101,
  #103).

### Upgrade notes

- **Read the four Breaking entries under Security before upgrading:**
  `JWT_SECRET` (at least 32 bytes) is required by the API, the worker and
  `phoenix-probe-admin`; the first web sign-up needs the setup token from the
  server log; the split web pod listens on 8080; compose and Helm default to
  production mode (strict CSP, CORS deny-by-default, no bootstrap password).
- Monitors and notifications owned by non-admins can no longer reach private
  addresses unless they are listed in `OUTBOUND_ALLOWED_CIDRS`. A startup
  warning counts the affected monitors. Non-admins can no longer create or
  reconfigure `docker` or `database` monitors; existing ones keep running.
- Hub migrations 076–089 and edge migrations 015–017 run at startup. Back up a
  populated MariaDB first and stop old writers during the migration, as for
  0.5.0.
- M7 aggregate paging and M8 SSH provisioning stay off until
  `PROBES_AGGREGATE_PAGING_ENABLED` / `PROBE_SSH_INSTALL_ENABLED` are set.

## [0.5.1] — 2026-10-06

### Fixed

- Monitoring and release regressions (#52–#67, #69), including migration 075.

### Changed

- Go module updates (#68); chart version 0.5.1 (#70).

## [0.5.0] — 2026-10-04

### Added

- **Opt-in multi-region probes.** Autonomous probes continue checks and
  source-owned alert delivery while disconnected, then synchronize configuration,
  current state, and durable telemetry with the hub. Includes protected enrollment,
  credential/certificate rotation, watchdogs, and recovery operations. (#49)
- **Regional monitoring and fleet administration.** Regional health/history,
  probe assignments, diagnostics, source-aware alerts, and backup/config admission.
  Helm supports probe configuration for API/worker deployments, and releases now
  include the standalone probe image and operator binaries. (#49)

### Fixed

- Hardened MariaDB migrations and concurrent monitor changes, deterministic
  latest-heartbeat ordering, and indexed history queue selection. (#49)
- Migrated chart layers to LayerCake 11's reactive context and synchronized the
  Bun lockfile. Browser coverage verifies rendering, resizing, range changes,
  and tooltip values. (#48)

### Changed

- Updated Go dependencies and Docker QEMU setup; preserved CGO-free builds. (#45, #48)
- Consolidated validation results and reproduction guidance; environment-specific
  UAT/load scripts and raw artifacts are kept outside version control. (#50)

### Upgrade notes

- Probes remain disabled by default. Enabling them requires an installation-key
  Secret; follow the [operator requirements](docs/multi-region/M6_OPERATOR_REQUIREMENTS.md).
- Before upgrading a populated MariaDB installation, back up the database,
  preserve credentials/PVCs, and stop old application writers during migration.
- Delayed or incomplete historical uptime is an accepted limitation. Full
  operational acceptance, including representative off-host restore and the
  24-hour fault/recovery campaign, remains incomplete; see the
  [validation summary](docs/multi-region/M6_VALIDATION_REPORT_2026-10-04.md).

## [0.4.5] — 2026-09-10

### Performance

- **Faster admin navigation.** Dashboard and monitors keep their last-good
  payload across route changes, and folder status uses a single batched
  Insights request instead of one fetch per group. (#43)

### Changed

- **Truly-split Helm images.** `mode=api` / `mode=worker` / `mode=split`
  Deployments now pull `ghcr.io/fiztoz/uptime-phoenix-api` and
  `uptime-phoenix-worker` (empty repository/tag/pullPolicy still fall back to
  the all-in-one `image.*`). The worker image is built with `-tags noweb` so the
  embedded SPA ships in the API binary only.

## [0.4.4] — 2026-09-08

### Performance

- **Faster Insights queries.** Independent database reads run concurrently, and
  group filters are pushed into SQL for both MariaDB and SQLite. A bounded,
  per-process 20-second cache coalesces repeated requests while checking current
  permissions and monitor metadata before serving results. Stage timings and
  cache counters are exposed through Prometheus. (#41, resolves #37)

### Changed

- Updated Go dependencies for OIDC, MySQL/MariaDB, Docker, cryptography, and
  SQLite. (#38)
- Updated `isomorphic-dompurify` to 4.1.0 and synchronized the Bun lockfile so
  frozen installs, frontend builds, and Docker builds succeed. (#40)
- Updated the pinned QEMU setup and GitHub Release actions. The existing tag
  binding and protected release approval flow are preserved. (#39)

## [0.4.3] — 2026-09-06

### Added

- **Non-admin UI now hides mutating controls it cannot use.** Create / edit /
  delete buttons on monitors and groups are gated on `is_admin || can_x`, so a
  read-only or capability-scoped user no longer sees actions that would 404/403
  at the API (matches the RBAC model established in the 2026-07 auth work).

### Changed

- **Release workflow parallelized across both phases.** `.github/workflows/release.yml`
  now fans out into a `prepare` → parallel dry-run legs (`binaries`, `chart`,
  `images`) → aggregate `dry-run` gate → parallel publish jobs (`all-in-one`,
  `split-{api,worker,web}` matrix, `chart`) → `create-release` topology instead of
  two monolithic jobs. All integrity guarantees are preserved per job via the new
  `./.github/actions/bind-release-ref` composite (SemVer check + `HEAD ==
  tag^{commit}` rebind on any publish path), and the `release` Environment still
  gates every publish job (one approval releases all). `scripts/release/dry-run.sh`
  gained a `STAGES` selector so CI can slice it; a bare local run is unchanged.
- **Dispatch defaults now build every artifact.** `build_all_in_one` and
  `build_binaries` default to `true` (alongside `build_chart` / `build_split`),
  so a `workflow_dispatch` publish matches a tag-push full release unless an
  artifact is explicitly unticked.

### Performance

- **Instant first paint on dashboard / insights / monitors.** These views render
  from cached data immediately and refresh lazily in the background instead of
  blocking on a cold fetch.
- **Cheaper latest-heartbeat probe.** The batched latest-heartbeat query prunes
  partitions, cutting scan cost on the `heartbeats` table.

### Fixed

- **Rollout checksums scoped per workload.** Helm rollout checksum annotations no
  longer share one value across workloads, so an unrelated config change stops
  forcing every Deployment/StatefulSet to restart.
- **MariaDB clients in in-cluster jobs no longer force SSL**, fixing connection
  failures for maintenance/migration Jobs talking to the in-release database.
- `insights-cache` formatting brought back under Prettier (frontend lint gate).

## [0.4.2] — 2026-09-05

### Added

- **In-release MariaDB (`mariadb.enabled=true`).** The flag now deploys an
  actual database: a 1-replica `StatefulSet` + `ClusterIP` Service
  (`<release>-mariadb:3306`) backed by its own PVC, with the official image's
  first-boot bootstrap creating the `phoenix` database and user. Server flags
  match what the schema needs (`utf8mb4` / `utf8mb4_unicode_ci` — the migrations
  emit no per-table charset — `--default-time-zone=+00:00` per the UTC rule, and
  `--skip-name-resolve`), readiness/liveness/startup probes use
  `healthcheck.sh --connect --innodb_initialized`, and every Phoenix Pod (all,
  api and worker) gates on an **authenticated** `SELECT 1` before starting.
  Tunables live under `mariadb.{image,auth,service,config,persistence,
  resources,waitFor,nodeSelector,tolerations,affinity,extraEnv,extraVolumes}`;
  `networkpolicy-mariadb.yaml` restricts 3306 to the release namespace; a second
  `helm test` pod probes the database. `values-internal-mariadb.yaml` +
  `make helm-install-mariadb` give the whole topology in one command.
- `make helm-validate` (also in `gate-full` and the CI `helm` job): renders the
  MariaDB topology and **asserts the objects exist** — the DSN resolves to the
  in-release Service, a StatefulSet/PVC/NetworkPolicy are present, the database
  Pod does not collide with the all-in-one Service selector, and the three
  ambiguous database configs still fail the render.
- `dbClientResolve` helper: maintenance jobs resolve `mariadb`/`mariadb-dump`/
  `mariadb-admin` vs the legacy `mysql*` names at runtime and shim the legacy
  names, so they work on `mariadb:11` (which ships only the new names).
- One-shot `dbMigration` Job: logical dump from an external MariaDB/MySQL
  into the in-release (or another) destination for cut-over.

### Fixed

- **`mariadb.enabled=true` was a stub that looked healthy** (AGENTS.md rule 7).
  It rendered a PVC and pointed `DB_DSN` at `<release>-mariadb:3306`, but no
  MariaDB workload or Service existed anywhere in the chart, so Phoenix
  crash-looped against a hostname that resolved to nothing while every endpoint
  and status-code check still passed. The all-in-one Deployment also mounted the
  database's RWO claim as an unused volume — which would have contended for it
  during a rollout. Both are gone.
- **A generated MariaDB credential could not authenticate to its own database.**
  `randAlphaNum` returns a new value per call, and the credential is needed as a
  literal in three templates (two Secret keys plus `DB_DSN` in the ConfigMap), so
  a first install wrote a DSN whose password was not the one the database was
  initialized with: the init gate passed and Phoenix died with `Access denied for
  user 'phoenix'`. The pair is now derived once per render (Helm shares one
  `.Values` map across templates in a pass) and each Secret key is retained
  independently on upgrade via `lookup`. The old retention lookup also targeted a
  `<release>-gen` Secret the chart never wrote, so it never retained anything.
- **The backup CronJob wrote empty backups and reported success.** `mysqldump`
  does not exist in `mariadb:11`, and `mysqldump … | gzip > file` reports *gzip*'s
  status, so the Job exited 0 on a 20-byte file. It now dumps to a temp file,
  refuses a dump with no `CREATE TABLE`, verifies gzip integrity, runs under
  `pipefail`, and keeps the credential out of argv (`MYSQL_PWD`). Verified live:
  a 61 KB real dump, and an unreachable host now ends `Error` instead of
  `Complete`.
- **The partition and rollup CronJobs could not run at all** on the same missing
  `mysql` binary; `job-db-migrate` also loses its `/bin/sh` for `pipefail` now
  that its `mysqldump | mysql` cut-over is checked. The partition job's `|| true`
  on the DROP loop no longer hides failures.
- CronJob, db-migrate, and helm-test pods no longer reuse Phoenix or MariaDB
  Service selector labels, so they cannot join those endpoints while Running.
- Helm tests retry the first `mariadb:11` 2002/115 from a new Pod, and the
  HTTP live probe, instead of failing on a single attempt.
- Handler test fake now actually persists `IncludeTarget` on re-Attach
  (`govet unusedwrite` / CI golangci-lint).

### Docs

- `charts/uptime-phoenix/README.md`: in-release vs external MariaDB, credential
  semantics, and why the standalone PVC is not a `volumeClaimTemplate` (removing
  a claim from the render makes Helm delete it).
- `docs/guides/helm-and-argocd.md`: render-only controllers have no `lookup`, so
  `mariadb.rootPassword` must come from a protected values source — MariaDB
  bootstraps users only on an empty data directory, so a re-rendered credential
  locks Phoenix out of an existing database.
- `docs/ARCHITECTURE.md`, `docs/DEPLOYMENT_MODES.md`, `docs/TESTING.md` updated;
  the chart README no longer claims the chart cannot run MariaDB.
- `values-internal-mariadb.yaml`: `helm --wait` hangs on WaitForFirstConsumer
  backup PVCs (k3s local-path); use an Immediate-binding class or skip `--wait`.

## [0.4.1] — 2026-09-03

### Added

- Notification channel detail page (`/notifications/:id`). Operators can see
  every monitor and folder attached to one channel, assign or unassign them
  there, and preview a sample message (template or provider default). Reverse
  assignment is `GET /api/notifications/:id/assignments` and is visibility-
  filtered. The leftover notifications list on Settings is removed.
- Per-monitor `include_target` on the monitor↔notification link (default on).
  Two monitors sharing one channel can include or omit the monitor target
  independently. The monitor detail page exposes a per-row toggle.
- Per-notification `include_ack_url` (default off). When a monitor goes down,
  Discord shows an Acknowledge button; other providers append a text link.

### Fixed

- Text notification senders no longer emit a dangling `Target:` line when the
  target is omitted (Slack, Teams, Feishu, SMTP, Mattermost).
- Public status pages show the Phoenix mascot instead of a broken `/icon.svg`
  (Uptime Kuma's stock logo path is not a Phoenix asset).

### Changed

- Container builds use `golang:1.27-alpine`. Go module minors were bumped
  (webauthn, mssql, mongo-driver, otel, grpc).

## [0.4.0] — 2026-09-02

### Added

- Read-only install-wide visibility: new `can_view_all_monitors` capability flag
  (Settings → Users → Access toggle + create-user checkbox). Admins already see
  everything via `is_admin`. Non-admins with the flag see every monitor and
  group — including ones created later — but still cannot edit anyone else's.
  Migration `032` defaults existing users off. OIDC IdP groups map onto it via
  `OIDC_CAP_VIEW_ALL_MONITORS_GROUPS`.
- HTTP monitors: `keyword_regex` matches the response body line-by-line
  (Go RE2). Complements the existing whole-body keyword search.
- Dashboard group drill-down now shows that group's immediate subgroup cards
  plus its directly assigned monitors, instead of flattening every descendant
  monitor. Subgroup cards still carry recursive totals and rolled-up status.
  Search, tag, status, type, or sort still produce the flat matching-monitor
  view.

### Changed

- Helm OIDC group/scope/grant fields accept a YAML list or a comma-separated
  string (`oidc.adminGroups: [phoenix-admins]`, `oidc.capViewAllMonitorsGroups`,
  `oidc.scopes`, `oidc.grantMap`, …). Lists are joined into the CSV env vars
  Phoenix already reads; existing string values keep working.
- Public status pages are served at `/status/:slug` (and `/status/:slug/history`).
  Custom-domain roots still rewrite onto that path.

### Fixed

- Unknown SPA paths such as `/ab` no longer render as
  "Status page not found or unpublished". They use the generic Page not found
  screen. Favicon and `/_app` URLs are root-absolute so nested `/status/:slug`
  pages keep the icon and bundle.
- Insights highlight cards paint as soon as they have enough data instead of
  waiting for full coverage.
- Dashboard and monitors group loading/error states no longer leave an empty
  folder view while groups are still fetching.

## [0.3.7] — 2026-08-24

### Changed

- Release workflow now supports a **selective, fast publish** via `workflow_dispatch`
  checkboxes (`build_chart`/`build_split` default on; `build_all_in_one`/`build_binaries`
  default off) gated by `publish=true`. Patch releases no longer have to rebuild every
  image and binary. Dispatch-publish still requires the `v<version>` tag to already
  exist, binds the build to that tag commit, and is gated by the `release` Environment
  approval; tag-push remains a full release of all artifacts. See `docs/RELEASING.md`.

### Fixed

- Extension iframe launch still returned `401 {"error":"missing authorization
  header"}` in v0.3.6 despite the session-cookie fix: `IssueSessionCookieOnBearer`
  wrote the scoped `phoenix_session` cookie *after* the catalog handler had
  already written its JSON body, which commits the response headers. On a real
  `http.ResponseWriter` the header block flushes on that first write, so the
  late `Set-Cookie` was silently dropped and never reached the browser — the
  iframe navigation then arrived with no cookie and no `Authorization` header.
  The cookie is now written on a `Response.Before` hook so it lands in the
  header block just before it flushes. The v0.3.6 unit test passed only because
  `httptest.ResponseRecorder` keeps its header map readable after the write; a
  new real-server (`httptest.NewServer`) regression test now asserts the cookie
  on the actual wire.

## [0.3.6] — 2026-08-24

### Fixed

- Extension iframe launch no longer returns `401 {"error":"missing
  authorization header"}`. The admin iframe loads
  `/api/extensions/:id/frame`, which is gated by header auth — but an
  `<iframe src>` navigation cannot carry the `Authorization: Bearer` header
  (the session JWT lives in `localStorage` and is attached only by the SPA
  fetch client). A scoped `phoenix_session` cookie (HttpOnly,
  `Path=/api/extensions`, `SameSite=Lax`, Secure under TLS) is now issued by
  the Bearer-authenticated `GET /api/extensions` catalog fetch, and the
  `/frame` route accepts the Bearer header *or* that cookie. The documented
  `ui_token` hand-off is unchanged: the redirect releases it once as
  `?ui_token=…` for the extension to swap for its own session cookie, and the
  token never appears in `GET /api/extensions`. The cookie is scoped to
  `/api/extensions` so state-changing routes (`POST /api/monitors`, etc.)
  never receive it — no CSRF surface is added.

## [0.3.5] — 2026-08-23

### Added

- Per-user extension visibility: new `can_view_extensions` capability flag
  (Settings → Users → Access toggle + create-user checkbox). Admins always
  hold it. Migration `031` backfills existing non-admins so their visibility
  is unchanged; users created after the upgrade default to off. OIDC IdP
  groups map onto it via `OIDC_CAP_VIEW_EXTENSIONS_GROUPS`.
- Token-guarded extensions: the admin iframe now launches through
  `GET /api/extensions/:id/frame` (same auth + view_extensions gate), which
  redirects into the extension path and — when the Helm value
  `extensions[].uiToken` is set — hands the extension's UI token over once
  as `ui_token=…` for the extension to swap for a session cookie. The token
  never appears in `GET /api/extensions`.
- Helm: `extensions[].uiToken` is one value for both sides — it also injects
  `UI_TOKEN` into the extension pod (via the managed Secret, `secretKeyRef`).

### Changed

- `GET /api/extensions` and the sidebar extension entries require the
  view_extensions capability (previously any authenticated user). Existing
  non-admins keep access through the migration backfill.
- Helm: the `PHOENIX_EXTENSIONS` catalogue lives in the managed Secret
  instead of the ConfigMap (it can now carry tokens); pods roll when
  `extensions[]` changes.

## [0.3.4] — 2026-08-23

### Fixed

- Insights no longer sits on skeleton for several seconds, then ranks every
  monitor as "No qualified data", after days of UP checks. The leading-
  transition query is now `LIMIT 1` per monitor (migration `030` index
  `(monitor_id, important, time, id)`) instead of loading the full important
  history. Empty `heartbeat_1h`/`1d` rollups no longer wipe a complete
  timeline. Startup catch-up fills 26h of 1m/1h and 90d of 1d so 24h latency
  has buckets after a restart.
- Extension sidebar icons loaded as `<img>` stay visible in dark mode
  (`dark:invert`) instead of remaining black against the dark nav.

## [0.3.3] — 2026-08-22

### Added

- Monitors page: Collapse all / Expand all for groups (including nested
  folders). Ungrouped monitors stay visible. The unused action disables itself.

### Changed

- Dashboard no longer fetches sparkline history for every monitor on first
  paint. Each card loads its last 60 beats only when it is near the viewport,
  with a small concurrency cap. Live heartbeats append to that card only.
- `GET /api/monitors/:id/heartbeats?limit=N` now applies the cap in SQL
  (`time DESC, id DESC LIMIT N`) instead of loading the whole `hours` window
  and truncating in memory. The monitors list page also paints without waiting
  on the tag catalog.
- Scheduler runs at most 8 monitor checks at once (a restart used to fire every
  due monitor in the same second). Database checkers open one session per
  probe (`SetMaxOpenConns(1)` / `MaxConns=1`) so mysql/mariadb monitors cannot
  exhaust the target's `max_connections` (Error 1040) while the API is serving.
- Access checks cache the user row (30s) and a non-admin's resolved visibility
  set (15s, dropped immediately on grant/revoke). Concurrent users no longer
  each hit MariaDB once per monitor on the dashboard. Heartbeats stay live.

## [0.3.2] — 2026-08-20

### Fixed

- Dashboard no longer hangs on skeleton cards when WebSocket `monitor.list` is
  delayed. The hub starts read/write pumps *before* building the snapshot (so
  pings keep the socket alive and a client close is detected immediately) and
  the snapshot frame waits for a send-buffer slot instead of being dropped
  (`client send buffer full, dropped monitor.list` → broken pipe). The UI also
  hydrates from `GET /api/monitors` so first paint does not depend on that
  single frame.

## [0.3.1] — 2026-08-19

### Added

- Database storage check: `storage_scope` (`database` default, or `instance`)
  on one monitor. PostgreSQL sums every non-template database; MySQL sums
  visible schemas. Still logical database size versus `storage_max_gb`, not
  host disk. Instance-wide PostgreSQL needs `CONNECT` on each database or
  `pg_read_all_stats`.

### Fixed

- Expired or missing WebSocket JWT no longer reconnects forever. The hub
  closes unauthorized sockets with `4001` (the range the dashboard already
  treated as logout). Pre-fix servers sent `1008`, which the client retried,
  so the UI sat on pending waiting for `monitor.list`. The client now treats
  both codes as a dead session. `JWT_EXPIRE_HOURS <= 0` is rejected at
  startup and clamped in the authenticator so a bad env cannot mint a token
  that is already expired.

## [0.3.0] — 2026-08-19

### Added

- Database monitors: optional session-pool and storage checks as capacity
  **conditions** (`ok` / `warning` / `error`, plus derived `stale`). Over
  threshold or a missing grant does not flip the heartbeat to DOWN, open an
  outage, or change Insights / SLA / public status. Warning, error, and
  recovery promote after two consecutive samples with 5-point hysteresis.
  Chips, Needs attention, RBAC-scoped REST/WebSocket, and
  `capacity_condition` notifications are included. Fixed engine queries only
  (no operator SQL). Setup: `docs/guides/database-monitor-setup.md`.
- Dashboard and wallboard **Card: Capacity** view: session-pool and storage
  meters replace the ping sparkline when those signals exist; other monitors
  keep the response graph. Choice persists in `localStorage`.
- S3 / object-storage monitor (`s3`): signed `head_bucket`, `head_object`, or
  `get_object` against AWS, MinIO, and S3-compatible endpoints. Health only —
  no usage or quota probe. Bucket names may include `-` and `_`; `_` forces
  path-style addressing. Setup guide: `docs/guides/s3-monitor-setup.md`.
- Generic K8s extensions catalogue + iframe tab: Helm `extensions: []`
  (default off) renders per-item Deployment, Service, NetworkPolicy, and
  an Ingress path before `/`. Phoenix reads `PHOENIX_EXTENSIONS` (JSON
  `{id, title, path, icon}` only) and serves `GET /api/extensions`. Empty or
  unset → `[]`. Admin sidebar appends a tab that iframes the registered
  path and uses `{path}/icon.svg` from the plugin image (overridable
  `icon`, Puzzle fallback). Not a new monitor type; image, secretName, and
  credentials are never serialized.

### Fixed

- Monitor detail and dashboard no longer refetch in a loop until the API
  returns 429. `beginConditionSnapshot` reads `conditionSeq` and
  `applyConditionSnapshot` increments it; those calls are now untracked.

## [0.2.3] — 2026-08-16

### Added

- HTTP JSON assertions can accept a directly translatable JSONPath subset
  (`$`, child names, array indexes, and `[*]`) while continuing to use GJSON at
  runtime. The new `has_value` condition rejects missing or empty values without
  treating valid `false` and `0` values as empty.
- Helm: JWT signing key can be supplied as a stable `secret.jwt` value or an
  existing Secret so API/all-in-one pods do not mint a new key on every
  rollout (which logged everyone out). Helm CLI still auto-generates when
  neither is set.

### Fixed

- Pausing or resuming a monitor no longer clears its folder, proxy, owner,
  flags, retry policy, or weight. Collapsing a group no longer un-nests it
  or blanks its name. Omitted PUT fields stay as stored; explicit `null`
  still clears placement.

## [0.2.2] — 2026-08-15

### Changed

- Helm: empty `image.tag` / `web.image.tag` now default to `Chart.AppVersion`
  instead of `latest`. A chart-version bump rolls pods onto the matching
  published image. Override the tag only to pin a different image.
- Helm: the API HPA is created only when `hpa.enabled` is true.

### Added

- Helm: API, worker, and all-in-one Deployments now carry `checksum/config` and
  `checksum/secret` pod-template annotations. An Argo CD sync (or `helm upgrade`)
  that changes a chart-managed ConfigMap or Secret rolls those pods so they
  pick up the new env. The optional web and cloudflared Deployments hash their
  nginx ConfigMap and tunnel token the same way.
- Helm: API Deployment accepts extra environment variables from values.
- WebSocket hub now updates `phoenix_ws_connections_active`. The HPA Pods
  metric is a second opt-in (`hpa.wsConnections.enabled`) so CPU scaling works
  without prometheus-adapter.

## [0.2.1] — 2026-08-15

### Added

- HTTP monitors: optional JSON response assertions. Configure `json_query` (gjson path) plus `json_operator` (`exists`, `not_exists`, `equals`, `not_equals`, `contains`, `not_contains`) and `expected_value` when comparing. The monitor form groups these fields; Uptime Kuma imports that already have an expected value map to `equals`.
- Helm: `values-production-split.yaml` overlay for split API + sharded workers + in-release Valkey against an external MariaDB. The MariaDB DSN now honors `mariadbExternal.password`.

### Fixed

- Go toolchain `1.26.6` for stdlib CVEs (`GO-2026-6218` and related). `go.mod`, `GOTOOLCHAIN`, CI, and the release workflow are aligned.

## [0.2.0] — 2026-08-14

In-release Valkey EventBus (official `valkey` 0.11.0 subchart), wired to `REDIS_URL`. Chart and app version `0.2.0`. See the [v0.2.0 GitHub release](https://github.com/fiztoz/uptime-phoenix/releases/tag/v0.2.0).

## [0.1.0] — 2026-08-09

First public release. See the [v0.1.0 GitHub release](https://github.com/fiztoz/uptime-phoenix/releases/tag/v0.1.0).

## Historical notes (pre-versioned changelog)

The items below were written while the tree had no tags. They are kept for
searchability and are not a complete per-release delta.

### Added

#### Post–Sprint D / Phase 5 close-out (2026-07-27 – 2026-07-28)

- GitHub Actions **restored** (owner): `.github/workflows/ci.yml` (PR/main gate) and
  `release.yml` (dry-run + owner-gated publish). Local `make gate-full` remains required
  offline. See `docs/TESTING.md` and `docs/RELEASING.md`.
- OIDC PKCE S256 (always on when OIDC is enabled); `cmd/phoenix-config` CLI;
  reverse escalation assignment UI; MariaDB migration `021` white-label columns.
- Gate-debt sprint: Go lint / frontend format / `govulncheck` brought to a clean baseline
  under Go 1.25.12.

#### Sprint D — Scale & trust, Track C (2026-07-25)

- `LICENSE` (MIT) added at the repo root; referenced from `README.md` and
  `charts/uptime-phoenix/Chart.yaml` (`artifacthub.io/license` annotation) and
  `charts/uptime-phoenix/README.md`.
- `make gate-full`: the complete local pre-merge gate (build/vet/gofmt, `-race` tests,
  `golangci-lint`, frontend check/test/build/lint/e2e, `helm lint`/`template`,
  `govulncheck`, `git diff --check`). `make govulncheck` added standalone. (At the time
  of this track CI was absent; CI was restored 2026-07-28 and now also runs `actionlint`.)
- Rolling-deploy WebSocket soak (`docs/RUNBOOK.md` §4.1): reproducible procedure and
  measured result proving `uptime-phoenix-api` restarts lose no heartbeats while
  `uptime-phoenix-worker` keeps running, and that WS clients reconnect and resume
  automatically. Closes the Phase 3 / Sprint B carryover item.
- `Monitor.Weight` kept and **wired for display sort** (API list `ORDER BY`, UI). Earlier
  “inert field” notes are obsolete.

### Removed (historical — later reversed)

- `.github/` (CI workflows) removed 2026-07-25 (`9de75e9`) while the project was
  local-gate only. **Restored 2026-07-28** — do not treat “no CI” notes below that date
  as current. See `docs/TESTING.md`.

#### Sprint C — Visible wins (2026-07-19)

- Certificate-expiry alerts: opt-in `cert_expiry_notify`, exact TLS `NotAfter`, fixed 30/14/7 day once-per-threshold alerts with restart-safe state, maintenance suppression, and certificate-specific templates on all 11 notifiers (migration `013`).
- Maintenance IANA `timezone` for cron windows (default UTC), half-open `[start,end)` evaluation, form picker, and backup round-trip (migration `013`).
- Status-page email subscriptions: double opt-in JWT tokens, per-page active SMTP channel, public subscribe/confirm/unsubscribe, admin channel + subscriber UI, incident and maintenance fan-out mail; dormant webhook table preserved as `status_page_subscribers_legacy_webhook` (migration `014`).
- Public status page: optional `cert_expiry_date` / `cert_days_left`, `subscriptions_available`, and server-side OG/Twitter metadata injection into the embedded SPA shell.
- `PUBLIC_URL` config + Helm `config.publicUrl` for absolute links in subscription emails.
- Uptime Kuma importer (`cmd/kuma-import`): read-only converter from **SQLite or MariaDB/MySQL** (Kuma v2) to Uptime Phoenix `BackupDocument` JSON; see `docs/KUMA-IMPORT.md`.
- Release dry-run workflow (contents:read, no publish), multi-arch Dockerfile `TARGETARCH` fixes, `docs/RELEASING.md`.

- [`4220ea0`](https://github.com/fiztoz/uptime-phoenix/commit/4220ea0) Normalize HTTP monitor URLs on save; add a custom color picker and datetime picker to the admin UI.
- [`347f4cb`](https://github.com/fiztoz/uptime-phoenix/commit/347f4cb) RBAC: `can_create_monitors` / `can_create_groups` capability flags; shallow (non-recursive) group grants.
- [`a530e7b`](https://github.com/fiztoz/uptime-phoenix/commit/a530e7b) Status pages: grid dashboard style and three-level public health display.
- [`56ae31a`](https://github.com/fiztoz/uptime-phoenix/commit/56ae31a) Incident severity levels, with matching status display.
- [`6b80c11`](https://github.com/fiztoz/uptime-phoenix/commit/6b80c11) Premium dropdown redesign and multi-select filters.
- [`f192fa6`](https://github.com/fiztoz/uptime-phoenix/commit/f192fa6) SVG wordmark component, replacing text-based branding.
- [`027a15c`](https://github.com/fiztoz/uptime-phoenix/commit/027a15c) `avatar_url` support on the Discord webhook notifier.
- [`dcf2707`](https://github.com/fiztoz/uptime-phoenix/commit/dcf2707) Status pages: configurable dashboard style (full vs. pills — grid followed later).
- [`9050ceb`](https://github.com/fiztoz/uptime-phoenix/commit/9050ceb) Open Graph meta tags for Discord link previews.
- [`297d4a6`](https://github.com/fiztoz/uptime-phoenix/commit/297d4a6) Folder (monitor-group) alerting on the group's own rollup status; the RBAC grant UI actually rendered for the first time (functions existed but no markup called them); a real confirmation modal replacing `window.confirm()`.
- [`d4ae125`](https://github.com/fiztoz/uptime-phoenix/commit/d4ae125) Premium dark design system overhaul.
- [`95d7fbc`](https://github.com/fiztoz/uptime-phoenix/commit/95d7fbc) RBAC admin grant UI (backend), capability gating, atomic status-page monitor reorder.
- [`4b4fc94`](https://github.com/fiztoz/uptime-phoenix/commit/4b4fc94) RBAC: scoped monitor/group visibility, capability flags, dashboard filters, monitor tags embedded in the wire payload.
- [`db4b090`](https://github.com/fiztoz/uptime-phoenix/commit/db4b090)/[`8f6c0a7`](https://github.com/fiztoz/uptime-phoenix/commit/8f6c0a7) Monitor groups: introduced as single-level parent/child nesting, then replaced with first-class monitor-group entities (folders) the next day.
- [`f94c78e`](https://github.com/fiztoz/uptime-phoenix/commit/f94c78e) Backup: full config export/import with ID remapping.
- [`0d65d77`](https://github.com/fiztoz/uptime-phoenix/commit/0d65d77) Outbound proxy support (HTTP/HTTPS/SOCKS5), assignable per monitor.
- [`61e649b`](https://github.com/fiztoz/uptime-phoenix/commit/61e649b) Response-time chart data on the public status page.
- [`ffa4a2b`](https://github.com/fiztoz/uptime-phoenix/commit/ffa4a2b) Embeddable SVG status badges.
- [`13b75fa`](https://github.com/fiztoz/uptime-phoenix/commit/13b75fa) Status pages: password gate, custom CSS, tags, and a public chart (this commit also fixed the `PasswordHash` leak on the public endpoint — see Fixed).
- [`9c5473b`](https://github.com/fiztoz/uptime-phoenix/commit/9c5473b) Auth: disable open self-registration after the first user; admin-driven user management with RBAC.
- [`697f178`](https://github.com/fiztoz/uptime-phoenix/commit/697f178) Monitor ownership enforcement, chart API, detail-page state refinements.
- [`68cbfaa`](https://github.com/fiztoz/uptime-phoenix/commit/68cbfaa) Uptime-Kuma-style monitor detail observability, plus related auth fixes.
- [`2119fd7`](https://github.com/fiztoz/uptime-phoenix/commit/2119fd7) Monitor statistics API and UI components.
- [`8ab3041`](https://github.com/fiztoz/uptime-phoenix/commit/8ab3041) API key management and user authentication enhancements.
- [`b65c6bb`](https://github.com/fiztoz/uptime-phoenix/commit/b65c6bb) Endpoint to list notifications for a specific monitor.
- [`a03b706`](https://github.com/fiztoz/uptime-phoenix/commit/a03b706) Public push-monitor ingest endpoint with HMAC verification.

### Fixed

- [`8ac7011`](https://github.com/fiztoz/uptime-phoenix/commit/8ac7011) Persist WebAuthn authenticator flags correctly; unwrap public-key options that were double-wrapped.
- [`083d7b9`](https://github.com/fiztoz/uptime-phoenix/commit/083d7b9) Close out status-page dashboard code-review findings.
- [`fcfef6f`](https://github.com/fiztoz/uptime-phoenix/commit/fcfef6f) Normalize status-page dashboard styles server-side and return verified public access correctly.
- [`294eda5`](https://github.com/fiztoz/uptime-phoenix/commit/294eda5) Use the WebSocket monitor status as a fallback for status display when the initial fetch lags.
- [`8da6781`](https://github.com/fiztoz/uptime-phoenix/commit/8da6781) Soften the light-mode color palette for eye comfort.
- [`954fa36`](https://github.com/fiztoz/uptime-phoenix/commit/954fa36) `HeartbeatRepo.ListByMonitor` returned the wrong slice when ordered ascending — now returns the most recent beats correctly.
- [`ba083d6`](https://github.com/fiztoz/uptime-phoenix/commit/ba083d6) Mascot mark switched to a cropped vector; fixed login-page logo centering.
- [`c471b45`](https://github.com/fiztoz/uptime-phoenix/commit/c471b45) Status pages: return `409` on a duplicate slug instead of a raw `500`.
- [`2f2d551`](https://github.com/fiztoz/uptime-phoenix/commit/2f2d551) Closed a batch of silent-failure bugs: incident resolve, missing defaults, retention loop, API-key expiry.
- [`60d30b9`](https://github.com/fiztoz/uptime-phoenix/commit/60d30b9) Real uptime data on status pages, first-class maintenance status, and UTC normalization at the repository boundary (see AGENTS.md rule 6).
- [`2a1f41e`](https://github.com/fiztoz/uptime-phoenix/commit/2a1f41e) Made maintenance-window alert suppression actually suppress alerts (it previously did nothing); unblanked the response-time chart.
- [`718b7de`](https://github.com/fiztoz/uptime-phoenix/commit/718b7de) Return a snake_case `TagView` instead of the raw `domain.Tag` (wire-shape discipline).
- [`cd41470`](https://github.com/fiztoz/uptime-phoenix/commit/cd41470) Maintenance windows: scope to the authenticated user, enforce ownership, fix the wire shape.
- [`aab1375`](https://github.com/fiztoz/uptime-phoenix/commit/aab1375) Fixed MariaDB zero-timestamp insert failures (`ERROR 1292`) and backfilled the admin flag on upgrade.
- [`4873d26`](https://github.com/fiztoz/uptime-phoenix/commit/4873d26) Closed a cross-tenant authorization bypass in the monitor-ownership check.
- [`81d0873`](https://github.com/fiztoz/uptime-phoenix/commit/81d0873) API-key middleware now reads the context user ID via the `ContextUserIDKey` constant instead of a stray `"user_id"` literal.
- [`68cd43e`](https://github.com/fiztoz/uptime-phoenix/commit/68cd43e) Persist the monitor interval display correctly; bound the response-time chart range.
- [`06dc133`](https://github.com/fiztoz/uptime-phoenix/commit/06dc133) Corrected the join condition in `GetByMonitorID` for both MariaDB and SQLite.

### Changed

- [`692f928`](https://github.com/fiztoz/uptime-phoenix/commit/692f928) `Makefile`: add a `gate`/`gate-fast` target and stamp the build with a version ldflag.
- [`955f750`](https://github.com/fiztoz/uptime-phoenix/commit/955f750) gofmt drift cleanup in `internal/core` (`user.go`).
- [`fa612df`](https://github.com/fiztoz/uptime-phoenix/commit/fa612df) Badges: switch to `viewBox` for scalable, crisp SVG rendering.
- [`26b3b26`](https://github.com/fiztoz/uptime-phoenix/commit/26b3b26) Centralize monitor target extraction behind a single `Target()` method (domain/ws/notifier).
- [`7a4e077`](https://github.com/fiztoz/uptime-phoenix/commit/7a4e077) gofmt drift cleanup across Go files; frontend type-tooling fixes.
- [`90e8390`](https://github.com/fiztoz/uptime-phoenix/commit/90e8390) Fixed the dead "accepted status codes" UI path, exposed monitor advanced fields, unified heartbeat types.

### CI

- [`6568651`](https://github.com/fiztoz/uptime-phoenix/commit/6568651) Restored the GitHub Actions pipeline (`.github/workflows/ci.yml`): backend build/vet/gofmt/race-test + golangci-lint, a MariaDB-11 job that boots the binary, asserts every migration applies, and runs all three smoke suites against a freshly recreated database each, a Bun frontend job, and Docker image builds — closing the gap `c8c74be` opened.
- [`c8c74be`](https://github.com/fiztoz/uptime-phoenix/commit/c8c74be) CI workflows removed entirely. The gate and MariaDB smoke suites kept working locally but ran only when someone remembered — the exact gap `6568651` closes.

### Docs

- [`cb0ac05`](https://github.com/fiztoz/uptime-phoenix/commit/cb0ac05) Add the root `README.md` (the repo had none).
- [`4275196`](https://github.com/fiztoz/uptime-phoenix/commit/4275196) `docs/PROJECT-REVIEW-AND-ROADMAP.md`: full project review, refinement plan ("Wave R"), and forward roadmap ("Wave F").
- [`75e7560`](https://github.com/fiztoz/uptime-phoenix/commit/75e7560) Handoff doc: confirmed remaining bugs, a verification recipe, hard-won rules.
- [`04beb8d`](https://github.com/fiztoz/uptime-phoenix/commit/04beb8d) Handoff doc for the next agent: backup/export-import notes + hard-won rules.
- [`63cc05a`](https://github.com/fiztoz/uptime-phoenix/commit/63cc05a) Uptime Kuma parity audit; agent guidelines for auth, wire-shape discipline, and parallel work (the seed of `AGENTS.md`).

### Earlier foundation (2026-06-23 – 2026-06-30)

The first ~8 days, before the project settled into one-commit-per-fix conventional-commit
discipline. Commit messages here are broader and less atomic than the rest of this file, so
they are summarized rather than itemized:

- **Initial scaffold and bootstrap** — [`f567b47`](https://github.com/fiztoz/uptime-phoenix/commit/f567b47) `init`, [`bfb2080`](https://github.com/fiztoz/uptime-phoenix/commit/bfb2080) bootstrap-user functionality.
- **Phase 3** — [`7a453d6`](https://github.com/fiztoz/uptime-phoenix/commit/7a453d6) API/worker split, observability, hardening.
- **Phase 4, Sprints 9-10** — public status API + custom domains ([`1dc4824`](https://github.com/fiztoz/uptime-phoenix/commit/1dc4824)), notification settings CRUD + test-send ([`d5448cb`](https://github.com/fiztoz/uptime-phoenix/commit/d5448cb)), rollup/uptime `AggregateService` ([`270c5c4`](https://github.com/fiztoz/uptime-phoenix/commit/270c5c4)), i18n + sparklines + favicon badge + heartbeat API ([`122d9a9`](https://github.com/fiztoz/uptime-phoenix/commit/122d9a9)), LayerCake charts ([`74e6efd`](https://github.com/fiztoz/uptime-phoenix/commit/74e6efd)), responsiveness pass ([`0309082`](https://github.com/fiztoz/uptime-phoenix/commit/0309082)), general functionality/perf work ([`8e9a772`](https://github.com/fiztoz/uptime-phoenix/commit/8e9a772)), a switch to Bun for the dev/testing toolchain ([`5aa5d83`](https://github.com/fiztoz/uptime-phoenix/commit/5aa5d83)).
- **Phase 4, Sprints 10-12** — mobile UX, rollups, sharded scheduling, notifiers ([`20cde92`](https://github.com/fiztoz/uptime-phoenix/commit/20cde92)); the split deployment stack and its docs ([`b07df67`](https://github.com/fiztoz/uptime-phoenix/commit/b07df67)); the first premium dark console UI pass and split web tier ([`e410030`](https://github.com/fiztoz/uptime-phoenix/commit/e410030), [`5fdc69f`](https://github.com/fiztoz/uptime-phoenix/commit/5fdc69f)); a local Colima k8s redeploy script ([`63e3cbf`](https://github.com/fiztoz/uptime-phoenix/commit/63e3cbf)).
- **Auth, deploy, notifications groundwork** — WebAuthn passkeys, Cloudflare Tunnel, Grafana provisioning ([`3dd3f1c`](https://github.com/fiztoz/uptime-phoenix/commit/3dd3f1c)); split dev setup enhancements ([`973d0aa`](https://github.com/fiztoz/uptime-phoenix/commit/973d0aa)); test-notification endpoint ([`17d49ca`](https://github.com/fiztoz/uptime-phoenix/commit/17d49ca)); trimmed the notifier list down to the 11 supported providers ([`2a9057b`](https://github.com/fiztoz/uptime-phoenix/commit/2a9057b)); automatic alert dispatch wired to status transitions ([`d39bc67`](https://github.com/fiztoz/uptime-phoenix/commit/d39bc67)); linter-warning cleanup and typed context keys ([`4e11821`](https://github.com/fiztoz/uptime-phoenix/commit/4e11821), [`ca70b79`](https://github.com/fiztoz/uptime-phoenix/commit/ca70b79)).
- Also in this window: roadmap/testing docs tracking Phase 4 sprint status ([`8677075`](https://github.com/fiztoz/uptime-phoenix/commit/8677075), [`899424e`](https://github.com/fiztoz/uptime-phoenix/commit/899424e), [`1af4ffe`](https://github.com/fiztoz/uptime-phoenix/commit/1af4ffe), [`8324401`](https://github.com/fiztoz/uptime-phoenix/commit/8324401)).
