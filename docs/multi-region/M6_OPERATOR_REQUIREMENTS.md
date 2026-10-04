# M6 — operator requirements

Requirements an operator must satisfy before remote assignments are safe to
turn on. This is the egress, durability, clock, disk, backup and provider
bullet of [M6](IMPLEMENTATION_PLAN.md#9-m6--failure-validation-and-v1-activation).
It records what the running code enforces. It is not a measured load envelope,
a populated migration rehearsal, a kill-test record, or permission to enroll
a production probe. Those remain open. The canary still needs the operator's
own deployment approval.

Read [the fleet gate](M6_FLEET_ACTIVATION_GATE.md) before the first remote
member, and [the operator guide](M2_OPERATOR_GUIDE.md) for the enroll commands.
Numbers below are admission budgets and acceptance bounds, not throughput claims.

## 1. Who must be upgraded first

Remote activation is refused with `409 worker_fleet_unaware` while any live
sharded worker has not attested assignment ownership. Upgrade every worker that
will hold a lease, then assign. `phoenix-probe-admin assign` is gated the same
way. Restore and config-as-code import are not: a restored remote identity is
created disabled and credentialless, so import cannot start execution.

The gate cannot see a local-mode process (`WORKER_ID` unset). It also cannot
see a worker that joins after the write. Do not start an old binary beside a
fleet that already has remote members. Detection limits are in the
[fleet gate record](M6_FLEET_ACTIVATION_GATE.md).

Set `PROBES_ENABLED=true` on API replicas that serve management/enrollment
and on worker/all processes that own connectors. Every enabled replica needs
the same installation key; API-only mode does not start background connectors.
The process refuses to boot without `PROBE_SECRET_KEY_FILE`. The chart does
not generate that key; see [key provisioning](KEY_PROVISIONING.md) and the
[deployment compatibility](M4_DEPLOYMENT_COMPAT.md).

## 2. Egress and listener

The hub dials the probe. The probe never dials the hub, including on reconnect.
After the socket is up, both sides send. A private hub therefore needs outbound
TCP/TLS to each probe endpoint and no inbound probe port of its own.

| Rule | What the code does |
|---|---|
| Listener | Probe default `PROBE_LISTEN_ADDR=:8443` (`cmd/probe`). Configurable, including `:443`. |
| Routes | TLS `/healthz`, `/readyz`, `/ws/probe/enroll/v1`, `/ws/probe/v1` only. No admin UI, no registration API. |
| TLS | TLS 1.3 only, both directions (`edge_tls_manager.go`, `pinned_client.go`). TLS 1.2 is rejected. |
| Pin | Hub stores a fingerprint and fails closed on mismatch, expiry, or an empty pin. Trust comes from the operator channel that copied the fingerprint, not from fetching the certificate. |
| Credential | `Authorization: Bearer` on the runtime socket. A query string, a second Authorization header, whitespace in the token, or an `Origin` header is rejected (`edge_server.go`). |
| Subprotocol | `Sec-WebSocket-Protocol: phoenix.probe.v1` is required. A proxy that strips it fails the session. |
| Proxy | The hub dialer sets `Proxy: nil` and ignores `HTTP_PROXY` / `HTTPS_PROXY`. A TLS-intercepting proxy cannot satisfy the pin. Allow direct worker egress to the probe address. |
| Connector placement | Continuous connector ownership runs in `all` or `worker`, never on an API-only replica. API enrollment also dials the probe through `ProbeAdminService.Enroll`; allow the chosen endpoint from both API and worker pods (and the operator CLI host when used). |

On the probe host, allow inbound TCP on the chosen port from the hub API/worker
egress addresses. NAT is not that permission. DNS for the enrolled endpoint
must resolve to that listener from the worker pods. Optional SSH install is a
different port and is not part of V1.

The chart NetworkPolicy, when `networkPolicy.enabled=true`, allows hub egress
to DNS, TCP 80, TCP 443, MariaDB 3306, and the optional event-bus and
Cloudflare tunnel ports (`charts/uptime-phoenix/templates/networkpolicy.yaml`).
It does **not** open 8443, SMTP 587/465/25, or arbitrary provider ports. A
policy-enabled hub cannot reach a probe on the default listener, and it cannot
send folder or local SMTP alerts, until the operator adds those destinations.
Putting the probe on 443 reuses the existing HTTP rule; it does not relax the
pin.

## 3. Filesystem durability

Edge state lives in `PROBE_DATA_DIR` (default `/var/lib/uptime-phoenix/probe`).
Requirements the store actually applies:

- Local filesystem, exclusive to one process. A second process on the same
  directory exits. Two processes sharing a copied `edge.db` are unsupported.
  Do not put the directory on NFS or any shared WAL volume.
- `probe init` requires the directory mode `0700` and writes files mode `0600`.
  `run` never regenerates a missing identity or key. Restore the intact
  directory; do not recreate files beside retained rows.
- SQLite opens with `journal_mode=WAL`, `synchronous=FULL`, foreign keys on,
  `busy_timeout=5000`, and one connection (`store.go`). `FULL` is the release
  durability setting. Do not weaken it to trade latency for a guarantee the
  rest of the system assumes.
- Sequence, observation, regional state and notification intent commit
  together. A crash around that transaction leaves all of them or none.
- A pinned reader at the 16 MiB WAL admission threshold stops new writes
  instead of growing the WAL without bound. That threshold is not a disk quota:
  one transaction admitted just below it can still append frames
  (`storage_bounds.go`).
- Stream reset archives the previous epoch and fsyncs it before deleting live
  rows. A rename that is visible before the directory fsync is not treated as
  durable; the next attempt re-establishes durability before deletion.

Hub crash survival of an acknowledged batch is only as strong as the hub
database. The chart does not set `innodb_flush_log_at_trx_commit` or
`sync_binlog`. Weakening those on MariaDB weakens the "acked means durable"
guarantee. SQLite hub installs get the driver's commit behavior; do not treat
a SQLite hub as a substitute for a MariaDB durability rehearsal.

Redis is not on this path. Losing Redis loses the optional intra-hub event
bus, not acknowledged telemetry.

## 4. Clock synchronization

Accepted history more than 30 seconds in the future does not update live state
(`services.MaxFutureClockSkew`). Regional health renders that evidence
`UNKNOWN` with reason `clock_skew`. Watchdogs use monotonic elapsed time, not
a one-way arrival timestamp. Rows that cross the database are UTC; a
local-zoned write is wrong on MariaDB even when the DSN says `loc=UTC`.

Keep the probe host, the hub workers, and the database host within that 30
second bound of each other and of UTC. NTP is the operator's job — the binary
does not discipline the clock. The database clock authorizes live state; a
probe whose wall clock runs ahead of MariaDB can have a valid write rejected.
S3 checks surface `RequestTimeTooSkewed` as a clock problem, not a bucket
outage.

A backward step does not reuse sequence numbers. Sequence, not wall-clock
order, is the stream identity. Do not "fix" a skew by copying an old SQLite
file forward.

## 5. Disk sizing

These are admission budgets. They are not a measured production envelope, and
they do not cap the whole volume.

**Probe data directory**, at the default `PROBE_TELEMETRY_MAX_BYTES=536870912`
(512 MiB) and `PROBE_TELEMETRY_RETENTION_HOURS=168`:

| Budget | Value | What it is not |
|---|---|---|
| Accounted telemetry | 512 MiB, or 7 days, whichever comes first | Not the size of `edge.db`. Payloads plus conservative per-row overhead. Configurable from 1 MiB to 1 TiB and 1 hour to 365 days. |
| Page cap | `max(1 GiB, 2×telemetry max + 512 MiB)` → 1.5 GiB at the default | A SQLite `max_page_count`. A database that is already larger may reuse pages but must not grow. |
| WAL admission | 16 MiB checkpoint threshold | Not a filesystem quota. |
| Delivery queue | 64 MiB, fixed | `PROBE_DELIVERY_MAX_BYTES` is not an implemented setting. |
| Metadata ledger | 64 MiB, fixed | Separate from telemetry. |
| Stream-reset archive | up to 1 GiB during a reset | Temporary, beside the live database. |

Pressure is reported at 80%. Ordinary observations are evicted before
transitions, and the eviction writes a durable gap in the same transaction.
At the cap, new recording fails visibly. It does not discard unacknowledged
evidence and report success. A full queue stops the scheduler's durable path;
checks may still run, but they are not durably recorded.

Size the volume above the page cap plus one reset archive plus filesystem
overhead. At 1,000 monitors every 60 seconds a probe emits 1.44 million
observations a day before retries, so the byte cap is reached before the 7-day
cap. That arithmetic is a planning hint, not a benchmark.

**Hub database.** Remote observations land in the same partitioned `heartbeats`
table as local ones. Default raw retention is `HEARTBEAT_RETENTION_DAYS=180`.
The partition CronJob is off unless enabled; when enabled it keeps
`retentionMonths` (chart default 12) and drops older monthly partitions. Those
two controls are independent — enabling probes does not turn the CronJob on.
Each additional probe multiplies rows by its own interval. Do not reuse a
local-only PVC size as a distributed sizing proof. A
[synthetic rehearsal](M6_PARTITIONED_MIGRATION_REHEARSAL.md) measured migration
037 on 100k partitioned rows and observed a metadata-lock wait. A
[follow-up](M6_MIGRATION_RUNNER_REHEARSAL.md) ran the 038–074 tail through the
application migrator on that schema. Populated
**production-sized** migration lock impact and true peak disk growth are not
yet measured; do not run an untimed `ALTER` on a large partitioned table and
call it rehearsed.

The optional backup CronJob is off by default. Its PVC default is 20 GiB and
it keeps 14 days of `phoenix-*.sql.gz`. That is a logical dump of the hub
database, not an edge snapshot and not a coordinated cut.

## 6. Backup consistency

A hub backup and an edge data directory are not one consistent snapshot unless
the operator takes them that way. Recovery procedures exist for a skew between
them. They are not optional if the two copies disagree.

Hub export (`BackupDocumentVersion` 2) carries probe identity metadata and
assignment sets. It does **not** carry runtime credentials, sealed secrets,
the TLS pin, connector leases, or edge queues. Import creates an unknown
identity disabled and credentialless. It never mints a second live identity
and never reroutes a remote-only monitor onto the hub scheduler. Reenrollment
is `phoenix-probe-admin register` (adopts a disabled registration whose
key/name/location match) followed by `enroll`.

The installation key is a separate backup. Ciphertext in a retained hub
backup cannot be opened with a newly generated key. There is no re-encryption
workflow. Lose every copy of the key and the protected snapshots are
unrecoverable. All processes of one installation must use the same key.

**Restoring a hub** — full procedure in
[lifecycle/recovery](M4_LIFECYCLE_RECOVERY.md):

1. Quiesce connector workers (`PROBES_ENABLED=false` or stop the pods) before
   the restore. Renew or revoke connector fencing before the restored hub
   issues a command.
2. On reconnect the hub advertises its committed cursor. The probe resends, or
   declares `restore_loss` when the hub is behind the probe's retention floor.
   Lost data is not reconstructed.
3. Re-run clear-history for every monitor cleared after the backup. A pre-clear
   backup has neither the deleted rows nor the fence, so an old queue can
   resurrect them until the clear is run again.
4. Do not accept an enrollment token from before the restore. A probe whose
   credential did not survive reenrolls.
5. Resume workers only after cursors and fences match.

**Restoring an edge:**

1. Stop the probe first. Restore the SQLite directory and its TLS/key material
   together. A snapshot without `config.key` and the certificate is an identity
   loss; reenroll instead of inventing files.
2. An old snapshot carries old sequence numbers. Open a new stream epoch with
   `prepare-reset`, `probe reset-stream --plan-file`, then `activate-reset`.
   Do not copy a live database onto a second VM and start it.
3. Restart. Replay uses the new epoch. A clear-history fence still drops
   covered evidence; the fence is scoped by monitor, probe and generation, not
   by stream.

Restore the hub first, then each edge, then resume workers. An edge must not
talk a restored hub into reusing retired sequence space.

Operational rollback of a hub that has already accepted remote evidence is
roll-forward to a compatible binary. A down migration refuses while non-local
assignments, remote history, or scoped incidents would be discarded. Disabling
the feature does not make remote-only monitors run locally. Drain or export
before any old-binary rollback.

## 7. Notification-provider reachability

Direct monitor alerts are sent by the process that owns the assignment. A
remote DOWN is paged by the probe, not by the hub. The hub being up does not
deliver that page, and a hub outage does not stop it, provided the probe can
still reach the provider and its data directory is intact.

Folder channels are the opposite. They are not copied onto the probe. The hub
worker that committed the observation or the current snapshot sends
`group_notifications`. A partition that leaves the probe able to reach Slack
and the hub unable to reach Slack pages the monitor and misses the folder.
The reverse misses the monitor page and can still page the folder once the
evidence has been ingested.

Consequences:

- The probe's egress policy must include every provider used by a monitor
  assigned to it, not only the hub path. SMTP uses port 587 when the channel
  omits `port` (`internal/adapters/notifier/smtp.go`); a saved port replaces
  that default. Gotify, webhooks and the others use the URL the channel was
  saved with.
- The hub worker's egress policy must include every provider used by a folder
  channel and by locally executed monitors. The chart NetworkPolicy does not.
- Delivery is durable at-least-once. A crash after the provider accepts and
  before the local outcome commits can send a duplicate. That window is
  expected. Do not describe it as exactly-once.
- Provider I/O is outside the database transaction. Shutdown gives in-flight
  provider attempts ten seconds, then stops. It does not wait forever on a
  dead provider.
- Remote notifications omit public acknowledgement URLs. Acknowledge the
  mirrored incident on the hub. The probe must be connected for that command
  to apply; the UI can show pending until it does.
- A provider timeout does not block recording or replay. A full delivery
  budget does. Both are visible; neither is reported as a successful send.

## 8. What this record does not authorize

Still open, and not implied by satisfying the sections above:

- the section 13 matrix as one recorded run
- a populated, partitioned MariaDB migration rehearsal with elapsed time, lock
  impact and peak disk
- bounded load cases and a 24-hour backlog drain rate
- kill tests at every durable boundary
- a canary, a deliberate production partition, or any production enrollment

Enabling `probes.enabled` in Helm renders the worker flag and the key mount.
It does not enroll a probe, and it is not the canary.
