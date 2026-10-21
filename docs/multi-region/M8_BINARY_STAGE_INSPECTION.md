# M8 — operation-bound read-only binary staging inspection

Continuation after locally committed archive checkpoint `b9cb6ed`. The next
bounded slice reads a proposed systemd staging marker and binary over SSH/SFTP.
It supplies no staging writer, upload, importer, executor, endpoint, startup or
journal transition. No live remote action is authorized by implementing it.

## Why inspection before delivery

The parallel planning passes identified two constraints. A Docker archive cannot
be recorded as the existing unowned `image_cache` resource; the journal tuple
names a selected manifest, not archive bytes or an owned staging path. A stored
reservation is not current host authority. Therefore this slice does not map
archive copying or marker equality onto `applied`, ownership, a fence or cleanup.
It establishes a small read-only observation protocol a future delivery design
can use, without implementing `ProbeInstallResourceExecutor`.

The existing SSH preflight stays unchanged. New transport uses only the fixed
`sftp` subsystem and read-only SFTP-v3 operations, not a shell or arbitrary exec.
No external SFTP dependency is added: the bounded subset uses existing x/crypto/ssh.

## Frozen domain and port

Actual new files `internal/core/domain/probe_binary_stage.go` and
`internal/core/ports/probe_binary_stage.go` are the shared source of truth:

```go
type ProbeBinaryStageIntent struct {
    Identity ProbeInstallIdentity
    OwnerID string
    AttemptID string
}
type ProbeBinaryStageObservation struct {
    Intent ProbeBinaryStageIntent
    State string
}
type ProbeBinaryStageInspector interface {
    InspectBinaryStage(context.Context, domain.ProbeBinaryStageIntent) (domain.ProbeBinaryStageObservation, error)
}
```

Intent requires a complete valid existing systemd identity and canonical owner
and attempt UUIDs. It is NOT an operation receipt, current grant or replay token.
Docker/hidden image alternatives and caller paths are refused. `Directory()` is
exactly layout-1 resource 2: `/opt/uptime-phoenix-probe-<operation_id>`.
Binary filename is `Identity.Binary.ReleaseName()`. Marker filename is the fixed
`.phoenix-stage-v1`. `Marker()` defines exact secret-free LF-terminated bytes in
a fixed order, including the full identity, owner and attempt; maximum 2048 bytes.
`MarkerSHA256()` identifies those canonical bytes, not a publisher.

States are only `absent`, `conflict`, `content_matches`. `Valid()` checks shape,
not evidence. Observation intent must equal the requested complete value exactly.
`content_matches` means the named marker and binary bytes matched their expected
size/hash during reads. It does not establish an atomic snapshot, closed directory
graph, owner UID, exclusive use, host fencing, current DB authority, cleanup,
installation, runtime version, enrollment or successful configuration activation.
Another local actor can change files immediately; these are observations only.

## Service ownership

ONLY NEW `internal/core/services/probe_binary_stage.go` and its `_test.go`:

```go
func NewProbeBinaryStageInspectionService(p ports.ProbeBinaryStageInspector) (*ProbeBinaryStageInspectionService, error)
func (s *ProbeBinaryStageInspectionService) Inspect(ctx context.Context, intent domain.ProbeBinaryStageIntent) (domain.ProbeBinaryStageObservation, error)
```

Validate nil/typed-nil context/port/service and intent before I/O. Call inspector
once with the complete value; no retry, DB, real I/O or goroutine in the service.
Check cancellation after the port returns, refuse any error-bearing result and
require valid exact-intent observations. Failed/refused output is the zero value.
Return only bare `ErrProbeBinaryStageInput`, `ErrProbeBinaryStageInspection`, the
four existing SSH input/trust/destination/connect errors or the two context
sentinels. Unknown/private/wrapped port diagnostics map to the corresponding bare
allowlisted cause via errors.Is, otherwise bare inspection unavailable. Cancellation
after the port returns revokes observations. Mock-port tests only.

## SSH/SFTP adapter ownership and exact API

ONLY NEW `internal/adapters/provisioning/ssh_binary_stage.go` and its `_test.go`:

```go
func NewSSHBinaryStageInspector(ctx context.Context, privateKey []byte,
    hostFingerprint, clientFingerprint string, policy probe.EndpointPolicy) (*SSHBinaryStageInspector, error)
func (s *SSHBinaryStageInspector) InspectBinaryStage(ctx context.Context,
    intent domain.ProbeBinaryStageIntent) (domain.ProbeBinaryStageObservation, error)
```

Borrow key bytes synchronously for existing `NewSSHHostInspector`; keep only the
parsed ephemeral signer as that inspector does. Validate the public client
fingerprint against `ssh.FingerprintSHA256(signer.PublicKey())` before any network.
Require request host/client pins equal configured pins. No known_hosts/key path,
SSH config/agent/password/PTY/environment/exec/forwarding API. Reuse the unchanged
inspector `connect` helper: resolve once, validate every candidate against a copied
policy, dial numeric addresses only. Reuse its host-key verification before auth.
Tests may adjust per-instance lookup/dial through the contained inspector; do not
modify verified old files or introduce process-global test knobs.

Bound the entire operation to 30 seconds with the caller's earlier deadline.
Set a connection deadline, cancel by closing it, and join cancellation callbacks
and local SSH session/client work before return. This is not remote-termination
proof; requests are read-only. Remote stderr has a shared maximum 4096 bytes and
is discarded. Overflow closes the connection and returns a fixed inspection error.
Never return raw packets, remote errors/output, paths or credentials.

### Bounded SFTP-v3 subset

Request the exact `sftp` subsystem, send INIT version 3, require VERSION 3. Accept
at most 32 bounded advertised extension pairs but never invoke them. Packets have
big-endian length <=64 KiB (including type/payload), no zero length, exact framing.
Each normal request has a monotonically increasing ID; require the exact reply ID,
expected type and complete payload, with no trailing or duplicate fields. Read
chunks at most 32 KiB, require progress, never accept more DATA than requested.
Handles are nonempty and <=256 bytes. Do not allocate from unbounded declarations.

Only INIT, LSTAT (7), OPEN (3) with READ flag 1 and empty attrs, FSTAT (8), READ (5)
and CLOSE (4) are permitted. No WRITE, SETSTAT, MKDIR, REMOVE, RENAME, READDIR,
extension or other mutation. LSTAT `/opt` and the canonical operation directory:
missing either means absent; a non-directory/symlink means conflict. Missing only
a marker or binary under an existing directory means conflict. Permission denied,
unsupported operations, connection loss or malformed statuses are unavailable,
NEVER absent. Recognize NO_SUCH_FILE=2 only for those LSTAT decisions; EOF=1 only
for READ completion; CLOSE requires OK=0. Ignore bounded private status text.

Attrs accept size/uid-gid/permissions/access-modification-time flags (1,2,4,8),
require permissions, and require size for files. Refuse unknown/extended flags.
Directory type is POSIX 0040000; regular file is 0100000; symlinks/special files
are conflict and must not be opened. Before reading each file, LSTAT regular type
and exact size, OPEN read-only, FSTAT regular type and exact size again. This does
not close the LSTAT/OPEN race or prove inode identity; do not claim otherwise.
Hash every received byte on the client, up to exact expected size plus one, then
require EOF and exact size/digest. Marker uses canonical MarkerSHA256/length;
binary uses requested pin SHA256/Size. Changed size/hash is conflict. Close each
remote handle even on refusal where possible; successful observations require
acknowledged CLOSE. Transport errors override a would-be successful observation.
No remote file bytes escape or execute; no whole binary buffer is retained.

## Disjoint workers and integration

Main owns the frozen domain/ports/contract, domain tests, independent actual
service-to-adapter integration tests, documentation and all gates. Parallel
`grok-4.7` service and adapter workers own only the two new files each listed above.
They do not execute commands/tests/network, spawn, read credentials/home or mutate
git. Adapter unit tests may author loopback SSH/SFTP fixtures, but main executes
them. Serving-model metadata is not independently established by a model request.

No changes to existing domain/ports, snapshots/verifiers, SSH preflight, journal,
runner/store/schema, DB clocks/fences, bootstrap/HTTP/CLI, modules, Make/CI, frontend
or Helm. This is a separate read-only port, never a success-returning executor stub.

## Required acceptance / status

Implementation is complete; frozen verification is recorded below. Test the actual service->SSH/SFTP->framing
and read lifecycle with a task-owned loopback SSH server. Assert exact subsystem,
no exec/env/PTY/forwarding and no mutating SFTP messages; exact read-only paths;
wrong host pin before public-key auth, wrong client key before dial, destination
policy, DNS numeric dial, exact marker/binary content, all three observation states,
permission errors not absence, symlink/special files never opened, changed size,
foreign marker/attempt/host tuple, digest/EOF/oversize/progress/ID/framing/version/
attrs/handle/status errors, partial channel/handshake/read cancellation, bounded
stderr, no private diagnostics, context checks and joined local I/O. Mocks do not
establish actual SFTP framing. A simulated server is not real OpenSSH deployment
compatibility or production host-fencing acceptance.

Run frozen-source build/vet/format, focused and artifact race repetitions, full
race, CGO-free Linux cross-builds, intended assertion mutants plus untouched
control, unchanged Make/parser/shard gates and an exhaustive default skip audit.
Use the clean CI-equivalent source mirror/private GOCACHE. Native host runtime
plus Linux builds is not Linux runtime or new MariaDB acceptance. Preserve failed
attempts and all evidence; clean only proven task-owned scratch after joined users.
Actual staging/delivery/writes, host ownership/fencing, Docker import, executor,
enrollment/exact config, broad M7 MariaDB and M6/release gates remain open.

## Corrections and retained attempts

Main independently found and reproduced three adapter issues before acceptance:

- **Cancellation callback lifetime:** `stageSession.join` called the AfterFunc
  stop function but did not wait when it returned false. Closing the underlying
  socket had already unblocked SSH, so inspection could return while the cancel
  callback was still held. A real-connection Close wrapper made that ordering
  deterministic. A completion channel now joins the callback after local SSH
  and stderr work. This does not assert termination of a remote write/process.
- **Typed-nil contexts:** a context interface could be non-nil while containing
  a nil pointer whose methods panic. Reuse the unchanged snapshot nil helper
  before invoking methods, both in construction and inspection. Recover-and-assert
  tests make a regression an intended assertion failure rather than a process
  panic. No existing snapshot/preflight file changed.
- **Short writes:** packet header/payload lengths were ignored when Write returned
  a short count with nil error. Each length is now checked before treating framing
  as complete. No partial-frame retry or invented peer acknowledgement is used.

The ordinary loopback match path passed three repetitions while all three probes
failed in `lifecycle-repro-01`; no debugger was installed. Source tracing isolated
stop(false), interface-nil and ignored write-count branches. The corrected expanded
control passed 333 named cases across three race repetitions.

Initial test-only corrections are separate: the main loopback fixture had a brace
syntax error caught by gofmt; a worker test variable shadowed encoding/binary;
and a worker uppercase-invalid UUID used digits only, so uppercasing changed
nothing. No production validation was weakened to accommodate these fixtures.
Retained `control-01` had 55 named passes/four named or package failures;
`control-02` had 101/four; the three-repeat lifecycle reproduction had six passes
and ten named/package failure events. These are not accepted campaigns.

The first mutation campaign stopped at the short-header mutant: that fixture also
short-wrote the payload, whose check still refused the request. Main corrected
only the test to short-write exactly one selected call. The same original header
mutant then failed the intended assertion three times. The initial full campaign
was deliberately interrupted after exact owned-PID checks and retained, not counted
as acceptance. `run-02` re-froze the stronger test source and reran every Go gate;
production code did not change for this test-isolation correction.

## Frozen verification record

Final source/inventory SHA256:
`726989cd32f8cf36de67a103e0ada51144aa606701cc3a0dbb387c4ab9e2bc13`
(**1,502 files**, baseline `b9cb6ed`). A task-owned clean mirror copies every
tracked/nonignored project file, verifies frozen source bytes, uses the exact
backend-CI embed placeholder and private GOCACHE. Ignored dependency/tool trees
are outside that CI surface; no application scenario or package is omitted.

Executed in `run-02`:

| Gate | Result |
|---|---|
| `go build ./...` | Passed, 25.874 s |
| `go vet ./...` | Passed, 5.356 s |
| Five-package artifact race/count-3 repeat | **1,932 named passes**, including **321 staging**, zero failures/skips, 52.621 s |
| Exhaustive staging race/count-3 repeat | **333 named passes**, zero failures/skips |
| CGO-free Linux probe/admin builds, amd64 and arm64 | Passed, 14.099 / 13.802 s; binaries not executed |
| Whole-source `go test -race -count=1 -json -timeout 3600s -p 4 ./...` | **6,397 named passes**, including **111 staging**, zero failures, 1,973.937 s |
| Intended assertion mutants and untouched control | **13 caught**, then **111 passes**, zero control failures/skips |
| `make release-image-gate m6-backend-harness-gate` | **24 correspondence +4 release +27 dual-engine-parser +17 shard tests**, plus existing release controls |

The focused selector includes four codec tests not selected by the broader
artifact regex. Full audit accounts for **36 application packages: 23 passing,
13 no-test**. **547 skips =542 missing `TEST_MARIADB_DSN` +five known optional/helper**;
zero unexplained skips, malformed event lines or stderr bytes. This is default
no-DSN acceptance, not new real-MariaDB coverage. Existing broad M7 MariaDB and
M6/release/deployment gates remain open. `golangci-lint`/`govulncheck` were unavailable;
vet/format are fallback checks, not completed lint/vulnerability scans. Unchanged
frontend and Helm gates were not run.

Mutants cover callback joining, request pins before dial, host trust, read-only
OPEN flags, exact reply IDs, permission-versus-absence classification, client byte
hashing, size+1/EOF, CLOSE acknowledgement, stderr budget, exact service intent,
short-write framing and typed-nil context refusal. Compile errors, process panics,
races, timeouts and zero selected tests are not catches. A bounded read-only
Grok-requested reviewer found no concrete scoped blocker; its inspection did not
execute tests or independently verify serving-model metadata.

Evidence lives under `tmp/m8-binary-stage-verification/`, with final campaign in
`run-02/`; initial repros, failed mutation metadata and interrupted-stage outputs
remain. No live SSH host, system SSH daemon, SSH home/config/agent credential,
external registry or task-provisioned MariaDB/Docker resource was used. Existing
full-suite SQLite fixtures do not establish MariaDB acceptance. Only task-owned loopback
servers and generated in-memory test keys were exercised. Native filesystem and
network execution is Darwin/arm64; Linux cross-builds are not Linux/OpenSSH runtime
acceptance. No host ownership/fencing, cleanup authority, actual staging/upload,
executor, installation, runtime enrollment/config activation or new MariaDB proof.
Only identified task-owned scratch is eligible for cleanup after joined users and
identity checks; retained manifests/logs/frozen source are not disposable scratch.
The inspection/writer snapshot is now locally committed as `ce6e106`; its exact
verification evidence was reused without a commit-only rerun. Archive `b9cb6ed`,
snapshot `f1dd181` and build fix `326ceda` are also local only. No push,
deployment, image action or external
remote-host action was performed by this inspection continuation.
The subsequent [fresh-only local writer](M8_LOCAL_BINARY_STAGE.md) is a separate
filesystem-effect slice; it leaves these inspection Go bytes and historical
verification scope unchanged. Its own Linux/test-container evidence does not
retroactively establish OpenSSH compatibility, remote delivery or host authority
for this read-only checkpoint.

