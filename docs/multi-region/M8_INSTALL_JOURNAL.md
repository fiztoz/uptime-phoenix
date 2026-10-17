# M8 — installation intent and resource journal

Continuation after `54fdc44`. This slice is a **durable internal building block**,
not an installer, new HTTP/CLI operation, worker or success endpoint. It records
installation intent and fenced resource-action reports. No SSH, Docker, systemd,
artifact delivery, enrollment or config activation is performed by this slice.
The broad M7 MariaDB acceptance blocker remains separate and open.

## Frozen authority and identity

- `ProbeInstallIdentity` binds a canonical operation UUID, SSH address/login,
  verified host PUBLIC-key fingerprint, client PUBLIC-key fingerprint, actor,
  mode and requested artifact tuple. Owner UUID is selected by the admitting
  process, immutable in storage, and is not a credential or takeover token.
- Modes are `systemd` with a valid `ProbeBinaryPin`, or `docker` with a valid
  `ProbeImageSelection`. The unused artifact alternative is exactly zero.
  These are **requested pins**, not evidence that this journal verified bytes,
  contacted a registry or observed a remote runtime. Future submission must run
  the existing artifact preparer and independently recheck actual delivery.
- Runtime `probe init` generates probe/stream identity and an enrollment token.
  An installation operation UUID therefore exists independently of a probe ID.
  No preflight receipt, journal row or resource report is enrollment authority.
- Creation atomically commits the whole operation plus its canonical resource
  graph. Exact identity retries return original owner/times/history and never
  replenish authority. Changed identity conflicts. No owner takeover, renewal,
  automatic resume, resource adoption or retry of uncertain side effects exists.
- Each operation has a fixed 15-minute DB-clock lifetime. Existing-row mutations
  serialize on its stored row, validate the complete graph **before** reconciliation,
  sample DB UTC time **after** acquiring the lock, and check current stored
  owner, state and deadline in the transaction. Fresh creation timestamps its new
  queued receipt with the DB clock inside its atomic insert transaction; it grants
  no execution. Begin is separately fenced after locking. Timestamps use UTC microseconds;
  updated/finished stamps never rewind. No transaction spans external I/O.
- In-memory copies, queue ordering and a historical reservation are not proof of
  current ownership. A future executor must preserve remote isolation/ownership
  markers and join/cancel correctly. This DB journal alone cannot fence an SSH
  process on a host or prove that a reported remote resource actually exists.

## Canonical resource graph (layout v1)

All resource references are derived, not caller-controlled paths. The operation
UUID namespaces initial-install resources; no existing path/container/unit is
silently adopted. Existing parent directories must be independently checked by
an executor; this journal does not authorize creating untracked shared parents.

Systemd, in order:

1. `data_directory`: `/var/lib/uptime-phoenix-probe-<operation_id>` (owned)
2. `artifact_directory`: `/opt/uptime-phoenix-probe-<operation_id>` (owned)
3. `unit_file`: `/etc/systemd/system/uptime-phoenix-probe-<operation_id>.service` (owned)
4. `service`: `uptime-phoenix-probe-<operation_id>.service` (owned)

Docker, in order:

1. `image_cache`: exact selected `repository@sha256:<manifest>` (shared, never owned)
2. `data_directory`: the same operation-scoped host directory (owned)
3. `container`: `uptime-phoenix-probe-<operation_id>` (owned)

The data directory is intended to contain private runtime identity, protected
config key and edge storage. No secret material is stored in the journal. Docker
uses a private persistent bind directory; UID/permissions, mount target, runtime
port and daemon health remain executor acceptance, not facts inferred here.
Service effects, enable links, partial writes and ownership markers must be
verified by the future executor before reporting a resource action `applied`.
A record is not automatic cleanup permission, particularly for shared images.

## Lifecycle and immutable action receipts

Operations: `queued`, `running`, `resources_ready`, `cancel_requested`, then
terminal `failed`, `canceled` or `interrupted`. **There is no installed/success
state or completion method in this slice.** `resources_ready` is nonterminal;
only reported resource preparation is done, not runtime/enrollment/config proof.

Resources: `planned`, `in_flight`, `applied`, `not_started` or `unknown`.

- Begin requires exact stored owner, queued state and live deadline. A second
  begin cannot dispatch again.
- Reserve binds one canonical resource to one canonical attempt UUID. A new
  reservation commits `in_flight` before returning `created=true`. Its prior
  resources must already be committed `applied`; queue position does not count.
- Exact reservation retries return their existing receipt with `created=false`,
  including historical outcomes after expiry. They **never** authorize another
  external attempt. A different attempt ID conflicts. Attempt UUIDs are unique
  globally, and each graph also refuses duplicate attempts or a successor start
  before its predecessor finished. One attempt per resource per operation; no
  automatic rearm, resume or ambiguous-commit redispatch.
- Record validates exact stored owner/resource/attempt plus current state and
  deadline. It may record `applied` (empty code), `not_started` (`not_started`),
  or `unknown` (`outcome_unknown`). Record only after that invocation has joined;
  `not_started` means the executor genuinely never invoked the mutation.
- An exact completed-outcome retry preserves immutable resource history. Its
  expired parent may still reconcile before the historical result returns;
  unlike Create retries, this is not a promise of an unchanged whole graph.
  Different outcome/code conflicts. A late result cannot repair an expired or
  uncertain receipt. Last `applied` moves running to nonterminal resources_ready;
  cancellation intent remains cancel_requested even if all resources apply.
- Unknown outcome atomically interrupts the operation and preserves the resource
  uncertainty. No dependent work can start afterward.
- Queued cancel is terminal canceled (no begin grant ever existed); active cancel
  is intent only. Terminal canceled requires stored cancel_requested, the owner
  to join the executor and no in-flight/unknown action. Owner Finish requires
  a begun operation; never-begun work uses queued cancel or cold expiry.
  Failure also refuses unresolved actions. Exact Create retries return unchanged
  history without cold reconciliation; this is no grant, and Observe or a new
  authority transition reconciles expiry before any further authorization.
- Owner interruption records owner_interrupted and marks in-flight actions
  unknown. Cold expiry records deadline_expired and marks in-flight actions
  unknown atomically, even after process/credential loss. Historical applied
  actions remain applied; planned actions are never fabricated as executed.
- Operation failure codes: unsupported_host, artifact_rejected, resource_conflict,
  execution_failed, permission_denied, runtime_unverified, not_started, internal.
  Canceled is canceled; interruption is owner_interrupted, deadline_expired or
  outcome_unknown. Raw remote output and library diagnostics are never outcomes.

## Schema, downgrade and retention

Migration 084 on both engines adds `probe_install_operations` and
`probe_install_resources`, using normalized nonsecret identity/tuple fields,
known state/error allowlists, non-null guards and unique ordinal/resource keys.
The resource FK restricts parent deletion. There is no delete/retention/cleanup
port. A downgrade refuses every nonterminal operation (even expired but not
observed), every in-flight/unknown resource, and every applied owned resource.
Shared image-cache history alone does not confer cleanup ownership. Terminal
history with no held/uncertain/owned-applied resources may be explicitly removed
by the guarded down migration. Retirement/recovery is an explicit future protocol,
not an override or deletion added for the tests.

## Integration and verification scope

Domain/ports were frozen before parallel work. Main integrated domain/ports,
actual-DB lifecycle/schema/contention/lock tests, gate parser and documentation.
One worker owned only the new facade and its unit tests; the storage worker
owned only the new store, its dedicated test file and dual-engine 084 SQL.
No shared authority helper, existing M7 or preflight implementation was changed.

`NewProbeInstallJournalService(operations, resources)` returns a facade and
error. Its methods mirror the ports as `Create`, `Observe`, `Begin`, `Cancel`,
`Finish`, `ReserveResource` and `RecordResource`, with identical argument/return
shapes and context first. It validates input, the entire returned graph and
exact operation/owner/attempt/outcome identity, sanitizes repository corruption
or unknown errors to `domain.ErrInternal`, preserves fixed not-found/conflict/
context sentinels, and returns defensive resource-slice copies. A failed or
canceled post-commit acknowledgement never returns a fresh action grant.

The use-case facade is not wired into startup or an endpoint until an executor
contract exists. Test the facade against actual storage and independent database
connections. Required coverage includes atomic creation, exact immutable retries,
commit dependency, fencing, cancel/finish/expiry races, unknown outcomes, cold
reads, clock rollback, clock-after-lock, corrupt graph refusal and guarded down/up.
Negative overlays must fail intended assertions, not compiler errors/timeouts.
Real MariaDB tests must execute in a new task-owned local disposable DB, never
against existing DSNs/credentials. Dedicated leaf coverage fails closed when
configuration or expected real-engine execution is absent. Default/SQLite passes
cannot close the known broad MariaDB blocker.

## Executed evidence

The internal journal is committed locally as **`7b38761`** after image checkpoint
`54fdc44`; neither has been pushed. No deployment or live opt-in. The follow-on
[one-shot resource coordinator](M8_INSTALL_RUNNER.md) reuses this facade without
changing the journal schema, fencing or outcome contract; it still has no concrete
remote executor or installation-success endpoint.
The final 2026-10-17 run used Go 1.26.9 offline, sanitized child environments and
frozen source `dd297abdfd8d9e10ed7ce42e5e7516dfc046261100be205b0d159ad52441b072`
(1,459 source files). Source contents and inventory remained unchanged throughout.
See [the repository workflow](../TESTING.md#m8-internal-install-journal-real-db-no-remote-installer).

| Executed check | Result |
|---|---|
| `go test -race -count=3 -json -timeout 900s` on journal domain/service/repository selections | 399 named passes, 117 MariaDB pass events, zero failures/skips; all 34 exact required real-engine leaves passed three times |
| `python3 -B scripts/m6_dual_engine_gate.py --scope m8-install-journal` | Fresh disposable MariaDB 11: 133 named passes, 39 MariaDB, all 34 mandatory leaves, zero failures/skips/malformed events |
| Post-mutation original-source control (`go test -count=1`, same selections) | 133 named passes, 39 MariaDB, zero failures/skips |
| Negative assertion overlays | 17 caught by intended assertions, not compiler failures, panics or timeouts |
| `go build ./...`, `go vet ./...` | Passed |
| `CGO_ENABLED=0 GOOS=linux` builds with `GOARCH=amd64` and `GOARCH=arm64` separately | Both passed; produced binaries were not executed |
| Gate-parser and repository-shard offline regressions | 22 + 17 passed |
| `gofmt -l internal/ cmd/`, `git diff --check` | Empty/pass |
| `go test -race -count=1 -json -timeout 2400s -p 4 ./...` (default/no DSN) | 5,869 named passes, 23 packages, zero failures; 536 named skips; 14 packages without tests; 1,912.173 seconds |

The 536 named skips are **531 MariaDB-dependent cases without a DSN**, plus the
five known helper/optional-checker cases: `TestConfigGroupCycleHelperProcess`,
`TestDatabaseChecker_Check_MongoDB_RealServer`, `TestEdgeCheckCrashChild`,
`TestEdgeDiskFullCriticalCommit`, `TestTelegramSender_Send_DownSeverity`.
Default-suite parent passes are not real-engine coverage. Only the dedicated
scope/repeat/control establishes this journal's MariaDB coverage; the earlier
broad M7 MariaDB failure and full section-13 acceptance remain open. No new
whole-backend MariaDB run is claimed. `golangci-lint` and `govulncheck` were
unavailable; vet/format are fallback evidence, not a lint or vulnerability scan.
No frontend or Helm source changed, and those gates were not run.

Controls cover owner and exact in-flight attempt fencing with raw persisted
state assertions, identity conflict, no fresh replay grant, expiry uncertainty,
expiry reconciliation committed before another Observe, clock-after-lock,
cancellation intent, monotonic timestamps, canceled post-commit acknowledgement,
stored operation/resource precision, SQL NULL-code refusal and owned-resource
downgrade. The extra predecessor control checks the typed conflict contract:
removing that guard still rolls back through the independent graph validator,
so it is **not** evidence that the altered code issued an unsafe grant. A separate
owner-loss control detects falsely classifying an uncertain action as not_started.

Integration reproductions corrected four draft defects: SQL CHECK UNKNOWN
admitted NULL terminal codes; active terminal cancel omitted stored intent;
read truncation hid stored precision corruption; exhausted admission contention
invented permanent conflict. Direct SQL invalid-state controls, actually persisted
nanosecond seeds, schema DATETIME(6) assertions and bounded real contention now
exercise those mechanisms rather than accommodating them with fakes.

Evidence is private under `tmp/m8-install-journal-verification/`. The initial
three-repeat run exhausted its five-minute package budget without a named
assertion failure; it is incomplete, not accepted. A later 399-pass/117-MariaDB
repeat and a passing scoped gate were superseded when two raw persisted
in-flight owner/attempt assertions were strengthened. The inventory/content
guard stopped that pipeline before build. Failed, incomplete and superseded
runs remain retained alongside final `race-repeat-03`, `strict-scope-02`,
`mutations-01`, `mutations-02`, post-mutation control and `full-race-01` results.
Only the final frozen-source pipeline is full-run acceptance for this bounded slice.

After all task users joined, exact task/run labels and IDs were verified before
removing the owned container/volume, generated credential files and `.work`.
Two still-present baseline containers and the pre-existing cached image were
preserved. A third baseline was already absent before cleanup; the initial
safeguard stopped before deleting anything, and no removal time/cause is claimed.
Logs, manifests, overlays and the cleanup discrepancy remain retained.

Real image-build correspondence (including inherited config), actual delivery,
remote ownership/cleanup, async installer routing, bounded redacted progress,
authenticated enrollment/config activation and full M8/M6 release gates remain open.
