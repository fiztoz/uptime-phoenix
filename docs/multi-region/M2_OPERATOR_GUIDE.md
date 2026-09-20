# Edge runtime, configuration synchronization and ordered replay

M2 supplies an autonomous edge and authenticated management connection. HTTP,
TCP and DNS checks and direct notifications run from accepted local configuration
during a hub outage. The first M3 increment adds automatic remote snapshot
construction and durable application receipts. The ordered replay increment now
commits retained availability observations, incident transitions and delivery
outcomes to the hub. Fleet UI, configurable retention/gaps, current-state snapshot
recovery, remote commands and watchdog paging remain unfinished.

## Initialize the probe

Build `./cmd/probe`, `./cmd/phoenix-probe-admin`, the normal `./cmd/app` or
`./cmd/worker`, and `./cmd/phoenix-probe-key` with the repository Go toolchain.
The edge has its own SQLite schema and does not embed the hub frontend.

On the probe host, `probe init --data-dir /private/probe-data` explicitly creates
identity, TLS certificate/key, protection key and edge database. The directory
must be owned by the runtime user and mode 0700; files are mode 0600. It prints
`probe_id`, `stream_id`, `certificate_fingerprint` and a ten-minute
`enrollment_token`. Copy the token to a private hub-side file through the
operator's trusted channel. Verify the fingerprint through that same channel,
not an unverified network certificate.

Start `probe run --data-dir /private/probe-data --listen 0.0.0.0:8443`.
The only routes are TLS `/healthz`, `/readyz`, `/ws/probe/enroll/v1` and
`/ws/probe/v1`. A new unconfigured probe is live but not ready. A second process
using the same directory exits without taking over the first process.

`PROBE_DATA_DIR`, `PROBE_LISTEN_ADDR` and `PROBE_SECRET_KEY_FILE` are optional
edge environment equivalents. The default edge key is `config.key` in its data
directory. Hub and edge use separate protection keys. `run` never regenerates
missing identity/key material. Restore intact backups instead of replacing files
beside retained storage. `inspect` reads safe identity/progress while stopped.
`token` replaces an unused enrollment token while stopped; an already bound probe
cannot be silently rebound.

## Register and enroll from the hub host

Use the hub's normal `DB_ENGINE` and `DB_DSN`. If needed, provision its initial key
with `phoenix-probe-key init --file /private/hub/probe.key`. Never replace an
existing key. Every compatible worker must use the same `PROBE_SECRET_KEY_FILE`
and installation identity.

Public management addresses are permitted by default. Private destinations require
an operator policy file such as `{"allowed_cidrs":["10.24.5.0/24"]}` through
`PROBE_ENDPOINT_POLICY_FILE`. Use specific ranges. Link-local, metadata, multicast
and unspecified destinations remain rejected. DNS resolution and dialing share
one deadline and use the validated address. Redirects are rejected.

Run `phoenix-probe-admin register` with `--probe-id`, `--stream-id`, `--key`
(stable slug), `--name`, `--endpoint wss://host:8443/ws/probe/v1` and
`--fingerprint`. IDs and pin come from local initialization. This persists
encrypted runtime credentials and the trusted stream before network enrollment.
A matching retry retains the original credential. Changed identity, stream,
endpoint or pin cannot silently rotate an existing registration.

Run `phoenix-probe-admin enroll --probe-id UUID --token-file /private/token`.
The token file must be a private regular file. Tokens never appear in command
arguments, ordinary output, URLs, logs or hub plaintext storage. Enrollment can
run with connector workers already running: the one-use operator exchange is
independent of their runtime lease and revalidates the prepared registration.

If the enrollment receipt is lost, retain the prepared credential and start the
connector: runtime authentication with the same credential recovers the binding.
Do not regenerate either identity to retry. The hub marks credentials `active`
only after the runtime handshake and validated probe health. `active` does not
mean configuration applied or ready. `status --probe-id UUID` returns credential
state, latest **prepared** `revision`, durable `applied_revision`, and
`sync_pending`. Revisions are decimal strings. Applied configuration does not
prove current reachability or telemetry ingestion readiness.

## Assign and synchronize saved configuration

Create a monitor through the existing hub API, attach its direct notification
channels, then replace its complete assignment set with
`phoenix-probe-admin assign --monitor-id ID --expected-revision N --probes UUID`
(or a comma-separated set including `local`). This preserves the current health
policy and checks the assignment revision. Tombstones retain generations; never
guess a recreated generation.

Start compatible normal hub workers with `PROBES_ENABLED=true`. Only worker/all
mode claims connectors. Workers retain a database-clock runtime lease through
reconnect backoff; individual sessions advance their own generation. Failed
renewal cancels the socket; stale callbacks cannot release or update a successor.
Use a uniform worker version when adopting migration 058: older workers do not
understand the parent runtime fence. Downgrade cannot discard a persisted epoch.

The connector builds each remote probe's complete authorized saved graph before
connecting, then reconciles it at the 15-second lease-renewal interval. Monitor,
direct channel, template, proxy, maintenance and assignment changes are captured
from the database, including writes from other API processes and changes whose
in-memory hints were lost. Reconciliation persists encrypted immutable snapshots;
unchanged content retains its revision and ciphertext. A changed revision causes
a reconnect and bounded complete transfer. Concurrent edits may require the next
reconciliation; pending work is retained across a hub restart.

The edge currently supports HTTP/TCP/DNS and direct delivery. Unsupported monitor
types, certificate paging and active escalation fail publication visibly; the
last accepted edge configuration continues until a valid replacement arrives.
Remote ACK links are forced false without changing the saved local preference.
Watchdogs remain disabled. Removing an assignment publishes an empty replacement
when it was the probe's last assignment.

`config.applied` is emitted after edge commit. The hub validates its exact revision,
hash and assignment count and commits the receipt under the current connector
lease. A lost receipt or failed receipt write reconnects and retries the same
immutable revision. A send without a durable application receipt times out after
60 seconds. `status --probe-id UUID` shows whether desired and applied differ;
`state: active` describes credentials, not configuration completion.

## Manual snapshot diagnostics

`phoenix-probe-admin prepare --probe-id UUID --expected-revision N --file PATH`
remains available for a private complete V1 document. It validates graph,
installed extension semantics and current assignment generations before protecting
exact bytes. It is a diagnostic preparation command: automatic connector
reconciliation rebuilds from saved hub settings and can supersede a manually
prepared document. Edit the saved hub configuration for normal operation.

## Offline behavior and bounds

After activation, stopping the hub does not stop edge checks or direct delivery.
Retry state, incident identity and sequence survive an edge restart. An ambiguous
provider timeout can cause duplicate external effects: delivery is durable
at-least-once, not an exactly-once provider guarantee.

On reconnect the edge replays exact retained event bytes from its local durable
ACK cursor. Only a validated ACK for the sent batch atomically advances that cursor
and prunes those telemetry rows. Welcome and health never delete evidence. Lost
ACKs cause duplicate replay, which reads durable hub receipts without rewriting
history or triggering provider sends. A stale generation cannot prune the outbox. Storage failures close the session
for reconnect; hub `telemetry.retry` emission remains unfinished.

Telemetry and delivery queues retain their existing 64 MiB bounds. Delivery
history is not pruned by telemetry ACKs. A provider attempt reserves outcome space
before external I/O. At capacity, recording fails visibly and scheduler readiness
becomes unhealthy; evidence is not silently dropped. Configurable retention,
explicit gaps and delivery-history cleanup remain necessary for long-running fleets.

The hub authorizes exact retained configuration and assignment membership at the
observation time, within a seven-day history horizon. Accepted old-generation
history cannot become current state; future-dated evidence cannot refresh state.
Permanent rejections are durable `probe_telemetry_receipts` with redacted codes.
Those receipts have no pruning policy in this increment; migration 054 downgrade
refuses to discard them. Unknown/retired streams or a restored hub cursor below
already-pruned edge data block replay pending explicit gap/reset recovery. Preserve
both stores and identities; do not delete rows or reset cursors to force progress.

## Reproduce acceptance

`scripts/probe_runtime_smoke.py` takes `--app-binary`, `--probe-binary`,
`--admin-binary` and a new `--output` directory. `DB_DSN` must name a fresh local
MariaDB database ending in `_smoke`. It uses local targets and webhook recipients,
real enrollment/assignment commands, automatic source edits and durable receipts,
two hub workers and a standalone edge, then stops
all child processes. Add `--verify-replay --mariadb-container CONTAINER` to check
hub/edge durable cursors, offline mixed-event replay, exact observation sequences,
incident/delivery mirrors, zero hub send intents and a second cold restart. The
container option runs read-only queries against the same disposable database.
The report contains safe IDs, sequence/fence progress and
outcomes; private keys and stores remain in the private output directory.
