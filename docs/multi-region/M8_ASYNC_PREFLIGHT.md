# M8 — durable asynchronous SSH preflight operations

Implementation contract for the continuation after `85405b7`. This slice adds
admin-only asynchronous **discovery**, not installation. The earlier local CLI
remains available and unchanged. Full M8 installation, artifact integrity,
resource tracking, runtime enrollment and exact config activation remain open
at this discovery checkpoint. Follow-on [binary preparation](M8_PINNED_BINARY_ARTIFACTS.md)
and [Docker metadata selection](M8_DIGEST_PINNED_IMAGES.md) provide bounded offline
integrity prerequisites, not remote delivery or installation.
**Candidate status: implemented locally; broad dual-engine acceptance is blocked.**
Committing or publishing this candidate does not close that blocker or authorize
feature activation or deployment.

## Frozen boundaries

- Opt-in `PROBE_SSH_PREFLIGHT_ENABLED=false` by default; enabling it requires
  `PROBES_ENABLED=true`. API/all processes execute their own admitted work;
  worker-only processes do not accept or acquire SSH jobs. No Redis or extra
  service is required. Existing single-pod defaults remain unchanged.
- `ports.ProbeHostInspectorFactory` parses a transient key into the existing
  discovery-only inspector and returns its **public** key fingerprint. API trust
  accepts a verified host fingerprint only, never a server-local key/known-hosts
  path, an SSH agent, a password, or a general command.
- `probe_host_preflights` is separate from `probe_operations`: a host being
  inspected has no registered probe identity yet. Receipts contain only frozen
  request metadata, public fingerprints, actor ID, owner UUID, lifecycle fields,
  fixed error codes and supported compatibility facts. No private key, key-file
  path, token, password or raw terminal output is persisted.
- At most four active operations per API process (service constructor allows
  bounded capacities 1–16). The bounded slot is reserved before creating work.
  Only the creator dispatches after the row commits. A failed/ambiguous insert
  never dispatches SSH; any unexecuted committed row later becomes interrupted.
- The caller supplies a canonical UUID `operation_id`. Exact retries bind
  address, login, verified host fingerprint, authentication **public** key
  fingerprint and requesting actor. They return the original identity, owner,
  deadline and outcome without another dispatch. Changed identity conflicts.
  A new run after interruption requires a **new operation UUID** and key
  resubmission. No durable key or automatic replay/takeover is implied.

## Database and executor lifecycle

States: `queued`, `running`, `cancel_requested`, `preflight_passed`, `failed`,
`canceled`, `interrupted`. Only the last four are terminal. A passed preflight is
never called an installed probe or an applied configuration.

Creation sets UTC DB-clock `created_at`, `updated_at` and a fixed 90-second
`deadline_at`. MariaDB stores `DATETIME(6)`; SQLite stores UTC text at the DB
clock's supported precision. The owner UUID never changes; there is no renewal,
claim takeover or epoch reuse. No lock/transaction spans SSH or other network
I/O. Existing transaction/DB-clock helpers are reused.

Every stored transition serializes on the operation row and samples the DB clock
**after** acquiring the authority lock. Begin requires queued + exact owner +
unexpired deadline. Passed/failed finish requires running + exact owner +
unexpired deadline. Cancellation completion requires cancel_requested, the unexpired original
deadline, and the original executor to have returned (or genuinely never started). Owner shutdown
may finish interrupted after joining its inspector. All terminal outcomes are
immutable; only an exact same-owner outcome retry is idempotent.

Observe atomically reconciles any hard-expired nonterminal row to interrupted
before returning it. It never creates work. This makes cold/restarted status
reads honest without retaining a key or guessing that an SSH command finished.
Interruption is not proof of remote quiescence and must not authorize future
installer cleanup. Current discovery commands are nonmutating.

Cancellation persists intent on any replica. The originating executor observes
it by polling stored state at most once per second and cancels its SSH context;
only after its inspector returns may it persist canceled. If it disappeared,
expiry produces interrupted, not invented canceled/succeeded evidence. The
existing SSH 30-second limit and shared 4 KiB output cap still apply. Final
outcome persistence uses a separate bounded context after request/executor
cancellation. Shutdown stops admission, cancels and joins tracked work before
closing the database. Clock rollback cannot move updated_at before its stored
value; it grants no new execution, owner or renewal.

A downgrade must refuse nonterminal receipts, including expired rows not yet
reconciled. Explicit downgrade may remove terminal discovery history; stop all
API writers for schema changes. No installation/resource ownership is asserted.

## Frozen HTTP contract

Routes are registered even when unavailable and return typed 503 rather than
SPA HTML or fake success. They use existing session-or-write-scope-API-key
middleware **then RequireAdmin**, with no new permission dimension. All admins
can read/cancel these fleet-level discovery operations. Responses are no-store.

- `POST /api/probe-host-preflights`
- `GET /api/probe-host-preflights/:operation_id`
- `POST /api/probe-host-preflights/:operation_id/cancel`

POST body is an exact JSON object with five string members (unknown/duplicate/
case-variant keys, trailing JSON, oversized bodies and non-string fields refuse):
`operation_id`, `ssh_address`, `ssh_user`, `host_key_fingerprint`, `private_key`.
The actor comes from authenticated context, never the body. The body is at most
128 KiB and the transient unencrypted key at most 64 KiB. It is parsed only into
an ephemeral signer. Operators must protect the management connection with
normal hub/Ingress HTTPS; no raw request or SSH output is logged by this slice.

Explicit response view:

- `operation_id`, `kind` = `ssh_host_preflight`, `status`, `phase` = `host_preflight`
- UTC `created_at`, `updated_at`, `deadline_at`
- nullable `error` = fixed allowlisted `code` + fixed `message`
- nullable `preflight` = `os`, `architecture`, `docker_cli`, `systemd`, `privilege`

Selectors, actor/owner IDs, fingerprints, private keys and raw output are not
returned. Initial/in-progress submission returns 202 only after commit; exact
terminal retry returns 200 with original outcome. GET returns 200 for an
existing receipt. Cancel returns 202 for cancel_requested, 200 for an already
terminal receipt, and never relabels a completed preflight canceled.

Malformed input/ID/key/trust returns 400, over-limit body 413, changed identity
409, missing operation 404, disabled/unavailable/closed/capacity exhaustion 503.
Busy responses include bounded Retry-After. Errors never expose underlying
storage/SSH diagnostics or bound input. Corrupt stored receipts return 503 even
when their underlying cause is domain validation; they are not treated as a bad
client submission and are never repaired by GET, cancel or an exact retry.
Cancel takes no request payload.

## Ownership and verification contract

Main owns domain/ports, SSH factory, router/composition/config, documentation,
integration and executed gates. Supporting workers may own only their assigned
new storage/migration/tests, service/tests, or handler/tests files. No overlapping
source edits or independent commits. The integrator verifies reported work.

Required effect coverage: both engines' commit-before-return, immutable retries,
concurrent independent writers, cancellation versus finish, current owner/expiry,
DB precision/UTC, cold reads/expiry, invalid/corrupt rows, rollback and guarded
downgrade. Service coverage includes bounded admission, no dispatch before
commit/after ambiguous errors, exact duplicate dispatch once, request disconnect,
shutdown/join, cross-replica cancellation and independent final write contexts.
HTTP coverage includes actual auth gates, disabled surface, strict/secret-safe
binding/views and persisted lifecycle effects through the real service/store.
Production factory tests reuse real loopback SSH, with no existing credentials
or external host. No installation/real-host acceptance is inferred from fakes.

## Executed candidate evidence and acceptance limits

Artifacts are private and ignored under `tmp/m8-async-preflight-verification/`.
These results were captured for the continuation after `85405b7` on
`feat/m8-ssh-preflight`. Git publication does not establish broader acceptance.

- Final focused race control (`mutation-round-03/control-repeat.jsonl`): the M8
  domain, factory, service, handler, bootstrap, API, storage and lock-wait tests
  ran three times: **378 named passes, 45 MariaDB pass events, zero failures and
  zero skips**. The database was a newly created, task-owned local MariaDB 11
  container; loopback SSH used generated test credentials only.
- All **14 mandatory M8 MariaDB leaves** executed, including the real router /
  authentication / service / repository lifecycle and the authority-lock deadline
  test. Parser regressions now require these exact leaves rather than accepting
  parent passes, SQLite substitutes or skipped/failed engine cases.
- Seven overlays were caught by their intended assertions: missing begin-expiry
  fence; late cancellation completion; changed terminal retry outcome; rejecting
  saturated exact retries; canceled-before-inspector-join; clock sampled before
  authority lock; inspection before durable creation. Compiler errors, panics,
  races and timeouts were not counted as catches.
- The lock-wait test was strengthened after an initial mutation survived:
  changing the held row caused MariaDB error 1020 and an automatic transaction
  retry that sampled a fresh clock. The final test commits the deadline first
  and holds the **unchanged** row with `SELECT ... FOR UPDATE`, so a pre-lock
  clock sample is genuinely tested. Failed and superseded runs are retained.
- The broad strict dual-engine run (pre-final-tree, retained failed attempt)
  **failed**: 5,291 named passes, 11 named
  failures, five known skips, 191 MariaDB pass events and no MariaDB skips before
  incomplete coverage. It is not acceptance evidence. The first operative
  aggregate failure was independently reproduced against an isolated archive of
  committed `85405b7`, without this continuation. The disposable VM clock was
  ahead of the host. A test-only clock-boundary experiment progressed beyond
  the first assertion but encountered further operative provider/standby
  failures; it was **not adopted**. No M7 authority/provider production code or
  existing M7 test was changed to make this M8 candidate pass.
- Generated overlay `.go` copies initially contaminated `go ... ./...` despite
  `.gitignore`. They were preserved under dot-prefixed `.overlays/` directories,
  not deleted or hidden by weakening the test inventory. The earlier build/vet
  failures therefore remain failed harness attempts, not passing stages.
- Final frozen-source default/SQLite backend checks in `run-02/` **passed**:
  **23 test packages, 5,582 named passes, zero failures**. There were **502 named
  skips**: 497 explicitly lacked `TEST_MARIADB_DSN`, plus the existing MongoDB,
  Telegram severity, crash-child, isolated disk-full and helper-subprocess cases.
  Thirteen package-level `skip` events meant **no test files**, not skipped named
  tests; the evidence reducer records them separately. This is not a green
  MariaDB-wide result. Raw Go events are retained unchanged.
- Build, vet, 17 parser regressions, empty gofmt output, diff whitespace checks
  and CGO-free Linux amd64/arm64 builds all passed on that same frozen source.
  The final Go/SQL/Python/shell/module source manifest remained unchanged:
  `d047c6de69926358cbca3ae7e23610eb07512018c3daa358323ba987edc06825`.
- The task-owned MariaDB container and volume were removed and absence verified;
  the two baseline containers and cached image were preserved. Generated DB
  credential files were removed. No existing database or credentials were used.

The executed default-suite commands used the cached **Go 1.26.9** toolchain
with `GOENV=off`, `GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off`, a durable
private `.work` TMPDIR, and no MariaDB DSN:

```sh
go test -race -count=1 -json -timeout 2400s -p 4 ./...
go build ./...
go vet ./...
python3 -B -m unittest discover -s scripts -p test_m6_dual_engine_gate.py
gofmt -l internal/ cmd/
git diff --check
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...
```

The M8-specific repeated control used its separate task-owned MariaDB DSN and
`go test -race -json -count=3` with the M8/SSH-preflight selector across domain,
services, handlers, provisioning, bootstrap and repository packages. The exact
seven overlays, assertion outcomes and final control counts are preserved in
`mutation-round-03/`; recreating its DB requires new disposable credentials.

`golangci-lint` and `govulncheck` are unavailable locally. There is no claim of a
full lint/vulnerability/release/frontend/Helm gate, external VM provisioning,
installation, runtime enrollment or config activation. Broad dual-engine
acceptance and the existing M6 operational/release gates remain open.

## Implementation map

- Domain and ports: `probe_preflight_operation.go` in `internal/core/domain/`
  and `internal/core/ports/`.
- Executor: `internal/core/services/probe_preflight_operation.go`.
- Receipt store: `internal/adapters/repository/probe_preflight_operation.go`;
  migration `083_probe_host_preflights` in both engine migration directories.
- Factory: `internal/adapters/provisioning/ssh_factory.go`, reusing `ssh.go`.
- HTTP: `internal/adapters/http/handlers/probe_preflight.go` and `router.go`.
- Composition: `internal/bootstrap/probe_preflight_operations.go`, `config.go`
  and `run.go`.
- Actual API/storage/locking integration tests: `m8_preflight_api_test.go`,
  `m8_preflight_operation_test.go` and `m8_preflight_lock_test.go` in the shared
  repository package; adjacent unit tests cover the other layers.

## Remaining M8 work

The next [offline binary prerequisite](M8_PINNED_BINARY_ARTIFACTS.md) prepares
explicitly pinned, privately copied probe binaries; it adds no installer API or
remote authority. Docker artifact pinning, complete idempotent Docker/systemd
installation, protected persistent identity /
data / secrets, owned-resource and progress journals, and authenticated runtime
enrollment **plus exact successful config activation** before reporting installed
success. Discovery receipts do not authorize any of those steps.
