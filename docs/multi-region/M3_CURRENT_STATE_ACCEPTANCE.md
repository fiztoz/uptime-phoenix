# M3 current-state recovery increment

> Verification correction: earlier matrix commands supplied `MARIADB_TEST_DSN`,
> but this repository reads `TEST_MARIADB_DSN`. Those runs skipped MariaDB; their
> MariaDB claims are withdrawn. An immutable export of commit `a40f80b` then passed
> the **complete real MariaDB/SQLite matrix** in 221.381s with the correct variable:
> **187 MariaDB pass events and zero MariaDB skips**. Current-state, replay and gap
> acceptance groups all executed. See [machine-readable evidence](M3_CURRENT_STATE_DB_EVIDENCE.json).
> A new `TestMain` guard rejects the misspelled variable, even with no selected tests.

Baseline: `6b40df9`, 2026-09-20. This increment is part of M3; the entire
milestone is still incomplete. Final gate results are recorded below. No push or
deployment is included.

## Behavior and boundaries

The edge retains the exact latest observation with its regional state, separately
from the replay outbox. A coherent SQLite transaction captures applied config,
active assignment generations, source high-water sequence and retained current
evidence. ACK pruning and restart therefore cannot erase the state snapshot.

The sender uses the control queue, waits for hub confirmation of the applied
config, and permits one state transfer at a time. The initial durable state
receipt releases historical replay; later snapshots refresh every 15 seconds
even while history ACKs are withheld. Each transfer has a 60-second receipt
deadline. A state ACK never prunes history. Readiness/config changes wake hub
health immediately after the corresponding authority or durable receipt succeeds.

The hub reuses the DB-clock connector lease, installation, enabled-probe and
active-stream fence. Exact durably applied config membership authorizes entries.
Current state, omission barriers and the receipt commit together. An omitted
active assignment becomes UNKNOWN; older replay at or below the coherent source
watermark cannot resurrect its prior state. A genuine newer observation clears
the barrier. Initial watermark zero is valid without inventing an observation.
Sequence orders same-stream evidence across backward clock steps. Equal-sequence
snapshots may change incident references but may not rewrite the observation.

This path creates no historical observations, incident mirrors or provider work,
and it leaves the replay cursor unchanged. Availability is the supported edge
payload; nonempty condition/TLS state is rejected explicitly. The receiver keeps
one latest receipt per stream. Receipt duplication, stale sessions, missing state,
wrong assignment/incident identity and late transaction failure have effect tests.

## Schema and recovery

- Edge migration `004_current_observation` retains exact event bytes. Upgrade
  backfills from retained outbox evidence. If the previous outbox already pruned
  that observation, the assignment is omitted until its next actual check.
- Both hub engines use migration `056_probe_current_state` for current metadata,
  latest receipts and missing-state barriers. New ping/message fields backfill
  from the exact prior observation so immediate same-sequence snapshots remain
  valid after upgrade.
- Downgrade refuses to discard durable state receipts or missing-state barriers.
  No stream is reset, no queue is cleared, and no credential is replaced.

## Validation ledger

- Focused TLS/state pump suite passed with race detection (19.114s), including
  current state before 300 queued events, periodic refresh while replay stalls,
  no history pruning from state receipts, receipt deadline and single transfer.
- Expanded SQLite state contracts passed (MariaDB was skipped in that invocation) (10.441s), including
  zero-watermark recovery, immutable evidence and incident-reference ownership.
- First SQLite repository matrix passed (MariaDB was skipped) (212.039s). That
  run preceded the final upgrade-backfill change, so a final matrix is required.
- Upgrade regression first failed on SQLite with `conflict`; after backfill it
  passed with race detection (3.614s). That follow-up matrix passed SQLite (203.384s) but also skipped MariaDB. The
  corrected immutable matrix above includes both upgrade regressions.
- Full probe adapter race suite after the health-wake correction passed (36.320s).
- `CGO_ENABLED=0 go build ./...` and `golangci-lint run` passed after the final
  corrections; lint reported zero issues. `git diff --check` passed.
- `go test -race -count=1 -timeout=20m ./...` passed (handler package 367.135s,
  repository 212.611s). The final health-wake and upgrade changes were then
  verified by rerunning their entire probe and repository suites above. Unchanged
  handler tests were not repeated after those bounded corrections.

Commands used `GOTOOLCHAIN=go1.26.6` and the existing temporary Go/lint caches.
The corrected run uses `TEST_MARIADB_DSN`. The disposable MariaDB DSN included `parseTime=true&loc=UTC&multiStatements=true`.
Logs: `/private/tmp/phoenix-m3-state-race-full.log`,
`/private/tmp/phoenix-m3-state-probe-final.log`,
`/private/tmp/phoenix-m3-state-mariadb-accepted.log`,
`/private/tmp/phoenix-m3-state-build-final.log` and
`/private/tmp/phoenix-m3-state-lint-final.log`. Corrected DB execution log:
`/private/tmp/phoenix-m3-current-a40f80b-real-matrix.jsonl`.

No frontend or Helm files changed in this increment. The final M3 product gate
and real 15-minute partition/command recovery acceptance are still required.

## Retrospective: three integration gaps

### Summary

Review and negative tests exposed a duplicate-receipt consumption race, delayed
configuration confirmation and an upgrade-only state mismatch in the integrated
draft. These were fixed before the current-state increment was accepted; they
were Codex integration defects, not evidence of an individual author's competence.

### Root cause and fixes

`edgeStatePump.handleApplied` originally relied on channel capacity for duplicate
suppression. Once the sender consumed the ACK, a duplicate could enter the empty
channel before `pending` was cleared. The following transfer could consume it as
its receipt. A mutex-protected `pendingReceived` bit now permits one enqueue per
transfer. `TestEdgeStateReceiptDuplicateCannotAcknowledgeNextTransfer` reproduces
that exact interleaving without timing guesses.

`sendHubHealth` already emitted an initial health frame, but `config.applied`
updated its confirmed revision without waking the sender. Current state and
backlog waited for the next 15-second heartbeat. A coalesced wake channel now
announces readiness and durable config confirmation promptly. The TLS regression
also asserts that a blocked receipt write cannot advertise the new revision.

Migration 056 originally introduced default ping/message values while equal-
sequence application compared those fields against the source. Empty-database
tests passed; an upgraded nonempty state rejected its unchanged source snapshot.
Backfilling exact prior observation fields preserves the invariant across upgrade.
`UpgradePreservesExactObservation` exercises down/up with existing evidence.

### Review and prevention

Antigravity reviewed the slice in conversation
`92475cca-166b-4d5f-8473-03d5dcfdb3d1`. Its command permission stopped broader
search, so its report is explicitly provisional and claims no test execution.
Codex accepted the latency consequence after reproducing it, but corrected the
report's inaccurate quoted control flow: initial health was already sent.
Findings require causal traces and exact source evidence, not reconstructed quotes.

The initial Codex approval review blocked external repository transmission; the
user explicitly approved the source/tests/design payload and Gemini destination.
Antigravity's later command denial was respected. Its follow-up used only existing
conversation evidence; no permission settings were changed or denial bypassed.
Verified fixes and these lessons were sent back with explicit no-edit ownership.

Required follow-up remains tracked in `M3_COMPLETION_WORK_CONTRACT.md`: historical
gap coverage and 1m/1h/1d/overall recomputation, both watchdogs, commands and offline
ACKs, rotations/reset, cleanup, bounded shutdown flush and full process acceptance.

### Verification-command retrospective

The suite deliberately skips MariaDB when `TEST_MARIADB_DSN` is absent. Codex
used a transposed variable name and inferred database coverage from package-level
success. The documentation already named the correct variable; this was an
integrator verification failure. No inference from elapsed time or a green
package line can establish that an optional engine ran.

The remedy is both immediate and persistent: the immutable accepted source was
rerun with the correct variable and explicit JSON pass events, historical claims
were corrected, and the test process now fails on the known incorrect variable.
The negative guard check passed even with `-run '^$'`; it therefore catches the
misconfiguration independently of test selection. Also inspect positive test
selection: one initial history command returned `[no tests to run]` because its
regular expression grouped across a slash. That result was discarded and the
intended cases were rerun with JSON events.
