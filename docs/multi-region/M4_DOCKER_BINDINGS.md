# M4 Docker resource bindings

This slice completes the missing Docker pull-checker path. It does not complete
M4's auxiliary state, lifecycle, backup, or deployment compatibility work.

## Operator configuration

Create a private regular file on the **probe host**, readable only by the probe
user (for example mode `0600`):

```json
[
  {"binding_key":"local-docker","kind":"docker_socket","endpoint":"unix:///var/run/docker.sock"},
  {"binding_key":"network-docker","kind":"docker_api","endpoint":"tcp://docker.internal:2375"}
]
```

Set `PROBE_RESOURCE_BINDINGS_FILE` or pass `--resource-bindings-file PATH` to
`probe run`. Keep that setting for commands opening an accepted configuration,
including `inspect`. The process reads the map once; changes require a restart.
The default remains an empty map with Docker capability withheld. No additional
service is required for non-Docker probes or a local-only installation.

Keys follow the existing V1 grammar, are unique across kinds, and are bounded to
128 entries. Socket endpoints must be absolute `unix:///…` paths. API endpoints
use the existing checker's plain `tcp://host:port` transport. Embedded credentials,
query strings, unknown fields, duplicate JSON keys, symlinks and public files are
rejected. TLS/client-certificate Docker transport is not implemented by the current
checker; HTTPS resources fail validation. Configure only Docker resources the
probe operator intends to expose to monitoring.

Configured resources are advertised by **key and kind only**. This establishes
transport configuration, not daemon health: an absent socket, unreachable daemon
or stopped container produces a normal DOWN check so that it can recover. Resource
configuration does not perform target I/O at activation.

Create a private hub-side assignment file containing references only:

```json
[
  {"probe_id":"11111111-1111-4111-8111-111111111111","binding_key":"local-docker","kind":"docker_socket"}
]
```

Use the existing local operator CLI:

```sh
phoenix-probe-admin assign --monitor-id 42 --expected-revision 1 \
  --probes 11111111-1111-4111-8111-111111111111 \
  --bindings-file /private/path/assignment-bindings.json
```

The result contains the saved revision, generation and `resource_bindings`, never
the local endpoint. Omission preserves bindings on retained assignments; an
explicit list replaces the complete binding set. An empty list clears bindings
only when the resulting assignments need none. New or re-created remote Docker
assignments require an explicit reference; a tombstone cannot revive one.
Reference edits advance the assignment revision while retaining its generation.
Membership removal/re-addition still advances generation.

The CLI can save desired state while disconnected. Verify keys against the
probe's operator-owned file. The authenticated hello checks the live advertised
inventory before transfer; the assembler and edge decoder independently reject
unknown keys or mismatched kinds before any activation. Fleet inventory storage
and the administrative HTTP/browser workflow remain M5.

## Storage and execution

Hub migration `065_probe_resource_bindings` adds key/kind columns to assignments
on both engines. Updates share the existing optimistic revision and configuration
lock order with membership, policy and assignment-history writes. Snapshots
automatically include the saved references. Docker's remote checker configuration
contains only `container`; the monitor timeout remains its explicit snapshot field.
A hub `docker_daemon` value is excluded without modifying local monitor settings.

Only the edge decoder inserts the selected endpoint into its in-memory checker
configuration. Hub publication, applied receipts, replay and watchdog readers use
the hub decoder, which validates references without resolving local paths.
Protected configuration retains the exact wire bytes. Failed replacement leaves
the previous configuration intact. Cold load re-resolves the map; a missing binding
prevents startup instead of falling back to the hub or default Docker socket.

Before downgrading migration 065, remove remote Docker assignments or export their
key/kind references. Its down migration drops those columns; membership, generations,
history and retained protected snapshots survive. Reapplying the migration does not
invent removed references, and publication remains blocked until they are supplied.

## Verification

Authored coverage includes TCP and Unix-socket Docker API execution; local map and
wire rejection; hub endpoint stripping; whole-configuration rejection and cold-load
validation; SQLite/MariaDB binding replacement, revision conflicts, concurrent
publication, tombstone handling and populated migration down/up.

The process harness accepts `--verify-docker`, combined with `--verify-replay` and
the existing disposable MariaDB options. It creates a monitor with an unusable hub
socket address, saves the resource reference through the real CLI, then verifies
successful edge execution and telemetry ingestion through its local mock Docker
API. Existing offline checks, process restarts and replay assertions remain active.

Commands executed:

- `CGO_ENABLED=0 GOTOOLCHAIN=go1.26.6 go build ./...` — pass.
- `GOTOOLCHAIN=go1.26.6 go test -json -race -count=1 -timeout=2400s -p 4 ./...`
  with `TEST_MARIADB_DSN` on disposable `phoenix_m4_docker_ci`, including
  `parseTime=true&loc=UTC&multiStatements=true` — pass in 762.974 seconds.
- `GOTOOLCHAIN=go1.26.6 /Users/fizto/go/bin/golangci-lint run --timeout=5m` —
  zero issues. The installed binary is outside the shell's default PATH.
- Fresh CGO-free app/probe/admin binaries through
  `scripts/probe_runtime_smoke.py --verify-docker --verify-replay` with a fresh
  disposable `_smoke` schema — 26 process stages passed in 68.222 seconds.
- `gofmt -l internal cmd/probe`, `git diff --check`, framework/driver-import
  inspection in production core code, and changed-document link checks — clean.

Engines and named tests exercised:

- SQLite plus MariaDB 11.8.9 in existing local container
  `phoenix-m04-checker-coverage`; new disposable schemas only.
- All authored tests above executed, including the exact
  `TestProbeResourceBindingContract/mariadb` and
  `TestRemoteConfigSyncContract/mariadb` cases.
- Real local Unix sockets and TCP listeners exercised the existing Docker checker
  against deterministic Docker API fixtures. The compiled runtime verified local
  resource resolution, actual scheduling, applied receipts, restarts and ingestion.

Passed / failed / skipped:

- Final full race command: 3,771 named passes, 22 tested packages, zero failures,
  316 MariaDB-named passes and zero MariaDB skips.
- Two existing optional tests skipped: `TestDatabaseChecker_Check_MongoDB_RealServer`
  and `TestTelegramSender_Send_DownSeverity`. Packages with no tests are excluded
  from named skip counts.
- The initial full run had one failing leaf test because its DSN omitted the
  documented `multiStatements=true`. The existing installation downgrade test
  reproduced the SQL syntax failure in isolation; adding that option alone made
  it pass. The subsequent full run passed. No production SQL change was needed.
- The process run replayed 41 retained events, preserving the original incident
  and two delivery outcomes with zero hub provider-send intents. The 29 changed
  Go/SQL/Python files stayed identical through the final gates and process run.

Acceptance criteria still unverified:

- Docker TLS/client-certificate transport (unsupported by the existing checker),
  and deployment-specific real daemon permissions or remote network policies.
- ICMP execution on a host permitting unprivileged ping.
- Remaining M4 auxiliary/lifecycle/escalation/backup/deployment compatibility and
  M5 administrative HTTP/browser workflows. The longer M3 outage, watchdog,
  command, rotation and reset process scenarios were not rerun for this slice.

See [hashed evidence](M4_DOCKER_BINDINGS_EVIDENCE.json) for source/binary/log hashes,
exact process stages and local artifact locations. This is local engineering
acceptance; nothing was pushed or deployed.
