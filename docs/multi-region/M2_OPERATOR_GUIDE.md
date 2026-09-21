# Edge runtime, configuration synchronization and ordered replay

M2 supplies an autonomous edge and authenticated management connection. HTTP,
TCP and DNS checks and direct notifications run from accepted local configuration
during a hub outage. The first M3 increment adds automatic remote snapshot
construction and durable application receipts. The ordered replay increment now
commits retained availability observations, incident transitions and delivery
outcomes to the hub. M3 also implements bounded retention/gaps, current-state
recovery, historical recomputation and bidirectional connection-watchdog paging.
Durable regional ACK commands are now available through the local admin CLI.
Operator credential/certificate rotation, explicit stream-reset recovery and the
actual fifteen-minute partition are accepted. Fleet UI and broader checker/auxiliary
compatibility remain outside this runtime; consult [current status](IMPLEMENTATION_STATUS.md).
For recovery commands and preservation requirements, see [explicit stream reset](M3_HUB_RESET_ACCEPTANCE.md).

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
Watchdogs are opt-in through the settings below. Removing an assignment publishes
an empty replacement when it was the probe's last assignment.

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
history or triggering provider sends. A stale generation cannot prune the outbox.
Retryable ingestion failures use `telemetry.retry` with durable progress; authority
failures close the session. Neither path claims an uncommitted ACK.

The runtime defaults to 512 MiB of telemetry and 168 hours of retention, configured
with `PROBE_TELEMETRY_MAX_BYTES` and `PROBE_TELEMETRY_RETENTION_HOURS`. Eviction records
explicit durable gaps. Delivery reservation and metadata have separate 64 MiB
budgets. A provider attempt reserves outcome space before external I/O. Periodic
cleanup retires eligible terminal history and old unreferenced metadata while
preserving unresolved/pending work and generation tombstones. At admission limits,
persistence pressure is visible rather than reported as success. Physical page/WAL
admission also bounds growth; see [storage bounds](M3_STORAGE_BOUNDS_ACCEPTANCE.md).

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


## Connection-watchdog settings

Migration 060 adds saved per-probe watchdog settings. Enabled remote snapshots now
require the negotiated `watchdog.v1` capability and explicit probe display metadata.
Both source runtimes persist incidents and send through their own accepted channels;
edge watchdog history replays without hub redelivery. Saving settings reports
`watchdog_saved`; it never reports an applied receipt or a healthy connection.

`phoenix-probe-admin status --probe-id UUID` includes a `watchdog` metadata object
with its own decimal-string `revision`. A new registration starts at revision
`"0"`, disabled, with 90-second loss, 30-second recovery, zero reminders and no
channels. Read this revision before replacing the whole settings object:

```sh
phoenix-probe-admin watchdog --probe-id UUID --expected-revision 0 \
  --enabled=true --notifications 12,34 --lost-after-seconds 90 \
  --recover-after-seconds 30 --resend-interval 5
```

`--enabled=true|false` must be explicit. Omitted `--notifications` clears the saved
channel set; omitted timing arguments use the defaults above. Resend intervals
are minutes. Unchanged complete settings retain their revision, including
revision zero when saving untouched defaults. A stale revision fails. Enabled
registration is required for writes; disabled registrations retain readable
settings. Registration also accepts `--location` alongside `--name`.

Channel deletion removes its saved reference and changes the next complete
snapshot without requiring a settings edit. Disabled channels and watchdog-only
templates remain in the complete dependency graph, even with zero assigned
monitors. Settings revisions are separate from complete config revisions and
applied receipts. Do not infer one from the other.

Each side pages after sustained loss of valid application health, and recovery
requires continuous healthy frames for the configured interval. A handshake alone
does not recover the incident. Disable closes an open source incident
administratively without a recovery notification. Remote ACK commands are still
unavailable; no notification contains a remote ACK link. See
[both-side process acceptance](M3_WATCHDOG_ACCEPTANCE.md).

Migration downgrade removes this saved intent, so stop config/runtime writers and
export the settings before rolling back 060. Both up/down migrations leave the
existing incident, source journal, telemetry and delivery tables intact.

## Acknowledge an original remote incident

On the hub, use the existing DB and `PROBE_SECRET_KEY_FILE` configuration. Local
access to that database and key is the operator authority; this does not add an
HTTP endpoint or a new permission flag. Obtain the original `source_alert_id` and
`assignment_generation` from its mirrored regional incident. Allocate one command
UUID and retain it for retries:

```sh
phoenix-probe-admin ack --probe-id "$PROBE_ID" --command-id "$COMMAND_ID" \
  --source-alert-id "$SOURCE_ALERT_ID" --assignment-generation "$GENERATION" \
  --actor "On-call operator" --ttl 24h
phoenix-probe-admin command-status --probe-id "$PROBE_ID" --command-id "$COMMAND_ID"
```

An optional `--note-file` must be a private regular file containing at most 4096
bytes. Actor names are nonblank and bounded to 256 UTF-8 bytes. Lifetime must be
between one second and seven days. Retrying `ack` must reuse the same command ID,
original target, actor, note and lifetime. It returns the original creation/expiry
and current receipt; changed options conflict.

The JSON `command.status` starts as `pending` with `remote_confirmed: false`.
**Remote alerts may continue while pending.** A socket write or mirrored ACK does
not confirm the command. The source's durable result changes that status to
`applied`, `already_applied`, `already_resolved`, `rejected` or `expired` and sets
`remote_confirmed: true`. A request whose lifetime elapsed can remain pending if
its earlier result was lost: retries recover the original receipt without applying
again. A source that never applied the request returns `expired`.

Commands are bound to the original source UUID, assignment generation and durable
stream. Reassignment does not redirect the ACK, and it cannot acknowledge a later
outage. A resolved original returns `already_resolved`. The first operator's ACK
metadata is retained when another command finds an already-acknowledged incident.
Remote notification links remain omitted; watchdog ACK is not implemented by this
positive-assignment-generation wire target.

Both hub and probe must include the command integration. The probe advertises
`command.alert_ack.v1`; older probes retain health/config/replay but do not receive
ACK commands, which stay pending. Hub request storage is bounded to 16,384 rows,
1024 unconfirmed commands and 64 MiB per probe. Confirmed requests and edge receipts
are retained through expiry plus 365 days; unknown results are not discarded to
make room. Reads expose metadata only, not encrypted payloads or operator notes.

The runnable process harness is `scripts/probe_runtime_smoke.py --verify-replay
--verify-command --mariadb-container NAME` plus its required binary/output options
and a fresh disposable localhost `_smoke` database. Its default command partition
is 15 seconds. `--command-partition-seconds` changes that duration; a short run is
not the complete fifteen-minute M3 acceptance.


## Rotate a remote runtime credential

Use the hub DB and protection key configuration above. Generate one rotation UUID,
choose a version greater than every previously issued version, and retain both:

```sh
phoenix-probe-admin rotate-credential --probe-id "$PROBE_ID" \
  --rotation-id "$ROTATION_ID" --credential-version "$NEXT_VERSION"
phoenix-probe-admin rotation-status --probe-id "$PROBE_ID" \
  --rotation-id "$ROTATION_ID"
```

The CLI generates and protects the token internally; it never accepts or returns
a plaintext runtime token for rotation. Reuse exactly the same rotation ID and
version after a lost response. Repeating issuance returns the existing operation,
its original command IDs and its unchanged ten-minute overlap deadline. Another
rotation cannot occupy that overlap, and a failed version cannot be reused.

`rotation.state` progresses from `preparing` to `activating` to `active`, or to
`failed` on a durable source rejection/expiry. Pending/healthy authentication alone
does not mean active. Preparation makes the candidate usable during the fixed
window; separate activation promotes it. After successful activation the new
credential remains usable, while the previous one expires at the original deadline.
If activation never commits, the original credential remains current. A request
whose result was lost stays retryable after expiry to recover its original result;
it is not automatically replaced or deemed failed by the hub clock.

Both peers must include `command.credential_rotation.v1` execution support. The hub
tries its protected candidate after confirmed preparation and can recover an
activation result after restart. Only a pinned HTTP authentication rejection
before websocket admission permits trying the saved current credential. Network,
key, pin and storage failures do not weaken that rule. After the receipt write,
the source quiesces further effects; the hub closes after durable confirmation,
with a bounded source forced-close fallback. Neither close nor timeout confirms
an operation.

`command-status` accepts the rotation's prepare/activate command IDs. `blocked`
means activation awaits preparation. `canceled` with `remote_confirmed: false`
means it was canceled locally after preparation failed; the source did not report
activation. Rotation tables and referenced command bodies are retained and bounded;
migration 062 refuses downgrade while a rotation, credential command or unpromoted
version high-water would be lost. Stop writers and preserve the hub key and edge
identity/database together for any operator migration/recovery work.

Use `scripts/probe_runtime_smoke.py --verify-replay --verify-credential-rotation`
with its required binaries, fresh private output directory and disposable MariaDB
configuration to exercise queued rotation, both-side restart and subsequent
telemetry. See [hub acceptance](M3_HUB_CREDENTIAL_ACCEPTANCE.md) and
[the retrospective](../postmortems/2026-09-21-m3-integration.md#credentials-and-certificates). Certificate rotation is described below; the implemented
[explicit reset workflow](M3_HUB_RESET_ACCEPTANCE.md) covers stream recovery.


## Certificate rotation

With the established hub DB/key environment, issue a new stable operation:

```sh
rtk proxy ./phoenix-probe-admin rotate-certificate --probe-id <uuid> --rotation-id <uuid> --certificate-version 2 --valid-for-days 365
rtk proxy ./phoenix-probe-admin certificate-rotation-status --probe-id <uuid> --rotation-id <uuid>
```

Choose an explicit UUID and a version greater than every attempted certificate
version. Reuse the exact UUID/version/validity on retries. The result's
`certificate_rotation` view includes public fingerprint/expiry, command IDs and
`preparing`, `activating`, `active` or `failed` state. Private keys remain protected
on the edge. Neither side generates a replacement identity on reconnect.

The hub queues only preparation initially; activation has a reserved ID and
capacity but no dispatchable payload until the prepared fingerprint is confirmed.
Only a durable activation result promotes the current hub pin. The same runtime
credential is resealed under it; credential version, stream and accepted config
do not change. Use `command-status` with each saved command ID to inspect actual
remote confirmation. A successful TLS handshake is not an activation receipt.

The fixed ten-minute window begins at command creation. Credential and certificate
rotations cannot overlap. After a lost activation result the hub tries the prepared
pin, including after cold restart and overlap expiry. It may retry the old pin
only after a pre-HTTP pin mismatch and while the original window remains open.
A source that never activated before expiry can require explicit verified
operator recovery; retries never extend trust or bypass pin/expiry checks.

Preserve the hub key/database and edge identity/key/database together. Migration
063 refuses downgrade while any certificate operation/command, expiry or version
high-water would be lost. Run certificate acceptance separately from credential
acceptance, because both respect the exclusion window:
`scripts/probe_runtime_smoke.py --verify-replay --verify-certificate-rotation`,
with all required binary paths and a fresh disposable MariaDB/output directory.
See [acceptance](M3_HUB_CERTIFICATE_ACCEPTANCE.md) and
[retrospective](../postmortems/2026-09-21-m3-integration.md#credentials-and-certificates).
