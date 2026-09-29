# M6 — fleet assignment-ownership gate (T34)

Slice of [M6 release validation](IMPLEMENTATION_PLAN.md#9-m6--failure-validation-and-v1-activation)
covering the requirement *"Verify all upgraded workers enforce assignment
ownership before enabling remote monitoring"* and verification matrix row
**T34 — Mixed-version workers during upgrade: remote activation blocked until
all workers honor assignments**.

Status: implemented and verified on both hub engines. This is **not** V1
activation approval and **not** a deployment authorization.

## 1. What was already true

An audit of every hub-side path that can execute a check found four, all already
guarded in the current binary:

| Path | Enforcement |
|---|---|
| `scheduler.LocalScheduler.poll` | `filterLocalRunnable` → `ExecutableByLocal` |
| `scheduler.ShardedScheduler.tick` | `filterLocalRunnable`, and `ClaimBatch` carries `LocalHubExecutionSQL` so a remote-only monitor is never even claimed |
| `handlers.PushHandler` | `ExecutableByLocal` resolves the generation; push monitors are local-only by contract, so no remote-only push monitor can exist |
| `handlers.validateMonitorConfig` | calls `Checker.Validate` only — no execution, not a path |

Two **write** paths also change where a monitor executes, and only one of them
was guarded before this slice: the admin API went through
`ProbeAssignmentService`, while `phoenix-probe-admin assign` wrote the desired
set directly through the assignment store. Both are gated now.

So the in-process property holds. What did **not** exist was any protection
against a *different binary*: during a rolling upgrade a worker built before
assignment ownership has neither the filter nor the claim predicate, and would
keep checking a monitor the fleet had just made remote-only, writing local
heartbeats that fight the regional evidence and can manufacture a false
recovery. Nothing observed or prevented that. T34 was an unchecked box with zero
coverage in `internal/`.

## 2. Mechanism

An older worker cannot be patched retroactively, but it *does* claim monitor
leases, and that makes it observable. Three pieces:

1. **Attestation** — migration `074_hub_worker_capabilities` adds a table keyed
   by `worker_id` holding the assignment protocol a worker enforces plus a
   `lease_until` bound. `ports.HubWorkerReadiness` is the port;
   `repository.HubWorkerReadinessStore` is one shared dialect-neutral Bun
   implementation (no engine-specific SQL to diverge).
2. **Declaration** — `ShardedScheduler.declareReadiness` attests
   `ports.HubWorkerAssignmentProtocol` with a TTL equal to its own lease TTL, so
   both expire on the same clock. It runs **before** the initial claim in `Run`
   and again on every `refreshAndClaim`. Order is the point: a worker that leased
   first would appear in the roster as an unattested executor and block remote
   activation for its own fleet.
3. **Gate** — `services.FleetActivationGate` is consulted by every remote-write
   entry point after input validation and before any write:
   `ProbeAssignmentService.Replace`, `MonitorService.CreateWithAssignments`
   (which is also the path `Clone` takes), and the operator CLI's
   `phoenix-probe-admin assign`. A set containing any member other than `local`
   is a remote activation; a local-only set is never gated, so a default
   single-pod install cannot be blocked by fleet state.

The CLI path was found while auditing callers of `ReplaceWithBindings` rather than
by design: it writes the desired set straight through the assignment store, so it
was an activation path the guarantee did not cover — and it is precisely the tool
an operator reaches for during a rollout. It now returns an operator-facing
refusal naming the straggler workers. `Restore` (backup import, config apply)
remains exempt; see §3.

`UnawareWorkers` reports two populations, both of which matter:

- a worker holding a **live monitor lease** with no current attestation at the
  required protocol — the mixed-version case;
- a worker whose attestation is **current but declares an older protocol** — it
  may hold no lease right now, yet it is alive, self-identified as unaware, and
  could claim work at any moment.

A dead worker is absent from both: once its lease ages past the lookback and its
attestation expires it executes nothing, so one crashed old pod cannot wedge
remote monitoring forever.

Refusals map to `409 worker_fleet_unaware`; an unreadable readiness table maps to
`503 worker_readiness_unavailable`. Both fail **closed**. The offending worker
identities are logged server-side and deliberately kept out of the response body,
which carries the documented fixed message.

## 3. Detection boundary (read this before relying on the gate)

Stated plainly, because an overstated safety claim is worse than none:

- **Only sharded workers are enumerable.** They own rows in `monitors.worker_id`.
  A local-mode worker (`WORKER_ID` unset) writes no lease and is therefore
  invisible. That topology is one process upgraded atomically, so it has no
  mixed-version window; local mode deliberately does not attest.
- **The lookback must cover the fleet's shard lease TTL.** `fleetLeaseLookback`
  derives it from `SHARD_LEASE_TTL` with a one-minute floor. A shorter window
  would let a running unaware worker age out of the roster and the gate would
  pass on false evidence.
- **It is a point-in-time check at write time.** It cannot stop an unaware worker
  that joins the fleet *after* a remote assignment was committed. That residual
  window is inherent to a rolling upgrade; the mitigation is operational — finish
  the rollout before activating remote assignments, not after.
- **`Restore` is exempt by design.** A backup import or config apply must succeed
  while the fleet is degraded or mid-rollout, and restored identities are created
  disabled pending reenrollment, so a restore does not hand live work to a probe.

## 4. Verification

All commands `GOTOOLCHAIN=go1.26.6`, MariaDB leg against a disposable
`mariadb:11` container. See
[M6_FLEET_ACTIVATION_GATE_EVIDENCE.json](M6_FLEET_ACTIVATION_GATE_EVIDENCE.json)
for hashes and counts.

- **Service gate** (`internal/core/services/fleet_activation_gate_test.go`):
  local-only sets never consult readiness (asserted by call count, not just by a
  nil error); an aware fleet commits; an unaware fleet is refused **and the
  writer is never reached**; an unreadable readiness table fails closed; an
  unwired zero-value gate fails closed on remote sets while still permitting
  local-only; the named-worker list is bounded.
- **Store, both engines** (`internal/adapters/repository/hub_worker_readiness_test.go`):
  unattested live lease blocks, attestation clears it, re-declaration stays one
  row, a second straggler is also reported, an aged-out lease stops blocking, a
  stale protocol blocks without holding a lease, an expired attestation blocks
  again, prune bounds the table, invalid input writes nothing, and migration 074
  round-trips. Leases are created through the real `ClaimBatch` path rather than
  hand-inserted rows.
- **Scheduler** (`internal/adapters/scheduler/fleet_readiness_test.go`): attests
  before claiming on both `refreshAndClaim` and the production `Run` entry point;
  a failed attestation is logged and does not stop scheduling; a hub with no
  readiness store schedules exactly as before.
- **Operator CLI** (`internal/bootstrap/probe_admin_test.go`): a local-only
  `assign` never consults readiness, an aware fleet is allowed, an unaware fleet
  is refused with the straggler named, an unreadable or absent readiness store
  fails closed, and `fleetLeaseLookback` floors a sub-minute `SHARD_LEASE_TTL`
  instead of producing a window that sees no live worker. The **call site** is
  guarded separately by a source-ordering assertion, because deleting the
  invocation from the `assign` command would otherwise leave every decision-logic
  test green while reopening the path.
- **Migration round-trip**: after `074 down` the readiness query must **error**.
  Returning an empty roster instead would report a falsely clean fleet and let
  activation through on a rolled-back hub.

### Mutation checks

26 mutations were applied to the six production files, each verified to be caught
by a named test and each restored with a SHA-256 comparison against the
pre-mutation file (0 missed, 0 unrestored; one anchor initially targeted the
wrong file and was re-run against the right one). They cover: local-time
attestation bound and local-time liveness cutoff; each of the three SQL clauses
that define "live", "attested" and "protocol current"; disabling the prune;
removing declaration validation; making declaration always insert; inverting the
gate's remote/local test; failing open on an unwired gate, on a readiness error,
and on a non-empty offender list; removing the message bound; deleting the gate
call from both `Replace` and `CreateWithAssignments`; removing attestation from
`refreshAndClaim` and from `Run`; removing the nil-readiness guard; attesting a
stale protocol or a wrong TTL; and for the CLI, deleting the gate call, moving it
after the write, making it always allow, unwiring the readiness store, and
removing the lookback floor.

**Harness defect found while doing this:** the first mutation run reported
*local-time attestation bound* as MISSED. It was not a blind guard — the run had
not exported `TEST_MARIADB_DSN`, so every MariaDB case silently skipped and only
SQLite executed. SQLite's driver normalizes a local-zoned `time.Time` on write;
MariaDB does not (a probe confirmed it stored `07:05:44` where UTC was
`00:05:44` on a UTC+7 host). Re-run with the DSN exported, the mutation is
caught by `TestHubWorkerReadinessAttestationIsUtcBound/mariadb`. This is the
documented `go test -run X` skip trap; the mutation harness now asserts the
MariaDB leg passes before trusting any result.

## 5. Remaining M6 scope

The operator requirements (egress, durability, clock sync, disk sizing, backup
consistency, provider reachability) are recorded in
[M6_OPERATOR_REQUIREMENTS.md](M6_OPERATOR_REQUIREMENTS.md). Still open: the full
section 13 matrix as a single recorded run, migration rehearsal on a
**populated partitioned** MariaDB with timing/lock/disk measurements, the
bounded load cases and 24-hour backlog drain rate, kill tests at every durable
boundary, and the canary. The canary and any production enrollment require the
operator's own deployment approval.
