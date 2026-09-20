# M3 watchdog source runtime verification

Base commit: `e3757bd`. This increment joins the source timer to durable storage
and application-health admission. Enabled-watchdog configuration remains guarded:
provider reconciliation and mirror replay authorization are still required before
operational watchdog acceptance. This document does not declare M3 complete.

## Behavior

`ProbeWatchdogSource` reads coherent source state, proposes on a copied timer and
publishes that timer only after the checkpoint/incident/outbox transaction commits.
A rejected health frame cannot contribute to later recovery. Concurrent ACK retries
preserve incident identity and acknowledgement while retaining already committed
recovery continuity. Firing resends use the current applied channel version without
allocating another incident transition. Disable resolves administratively with no
UP intent; re-enable starts a fresh grace period. A new process restores measured
loss, but must establish its own healthy recovery streak.

`ProbeWatchdogRuntime` outlives sockets. Receipt timestamp capture, bounded exact
wire validation/admission and timer reservation share a short mutex. Config reads,
source persistence, provider I/O, callbacks and WebSocket close run outside it.
Every queued health sample retains its original admission time and generation;
degraded samples are not coalesced away. Reconnect adopts a strictly newer durable
generation. A late prior reader/close cannot affect it. An independent disconnect
slot remains available when the health inbox is full. Transient failed work retains
its ordered time; stale generation work is discarded. Never-enrolled absence drops
the old tick so later enrollment cannot backdate arming.

Only completed source storage work advances the progress deadline. Before first
config, a successful read proving unarmed state can advance it. Missing config for
previously armed state fails closed. A hub parent stops renewal after 20 seconds
without progress, cancels and joins session/watchdog work, then releases ownership.
The edge runs the same source owner independently of its incoming session.

Both composition roots now construct the controller. The hub reads the exact
protected *applied* snapshot, including across a cold reader or config change;
it never substitutes a newer prepared graph. Parent ownership, registration,
installation key, snapshot hash and current active revision retain database fences.
The established WebSocket health worker keeps its DB checks and receipt semantics;
watchdog admission precedes potentially blocked work in either worker lane.

## Verification scope

Focused service regressions passed with race detection. They exercise exact 90/30
loss/recovery, rejected speculative health, atomic opening retry, concurrent ACK,
resend throttling/current channel version, administrative disable, fresh rearming,
restart identity/checkpoint, clock rollback, admission/tick order, stale readers,
queue overflow, stalled storage, delayed enrollment and cancellation/join order.

The new real-WebSocket tests hold both worker callbacks blocked while validated
healthy/degraded samples enter admission in order with the original timestamp.
Role, generation and exact-field alias checks still reject invalid input. The new
cold config-reader contract passed on SQLite and MariaDB, including applied versus
prepared graphs, missing active config, stale owner/epoch, disabled registration,
expired owner, wrong installation key and mismatched snapshot hash. The focused
adapter gate recorded 15 named pass events, including four MariaDB events, and no
skips or failures; counts include parent events.

The final full Go race suite passed in 22 tested packages, with 3,217 named pass
events and zero failures. The log contains 235 MariaDB-named pass events (198
lowercase `/mariadb` engine subtest events), no MariaDB skips, 13 packages with no
tests, and two unrelated named skips: the opt-in real MongoDB checker and the
legacy Telegram hardcoded-URL test. Named counts include parent events and some
SQLite cases named after MariaDB regressions; they are not independent scenario
counts. All named new MariaDB config-reader cases actually passed. CGO-free build
and lint with zero issues passed. The 18 changed Go source hashes match both the
full run and the real-process build. See
[M3_WATCHDOG_RUNTIME_EVIDENCE.json](M3_WATCHDOG_RUNTIME_EVIDENCE.json).

```text
Commands executed: go test -race -count=1 -timeout=20m -json ./...;
  CGO_ENABLED=0 go build ./...; golangci-lint run; git diff --check;
  scripts/probe_runtime_smoke.py --verify-replay --verify-history
Engines and named tests exercised: real SQLite and MariaDB;
  TestHubWatchdogAppliedConfigReader/<engine>/ColdAppliedGraphBeforePreparedGraph,
  AuthorityAndHashFences, RegistrationAndExpiredOwner;
  TestProbeWatchdogSource*, TestProbeWatchdogRuntime*, TestSessionWatchdog*,
  TestProbeConnectorWatchdogStallCancelsAndJoinsBeforeRelease;
  current app/probe/admin processes with two workers and a probe restart.
Passed / failed / skipped: 22 tested packages passed; 0 failures; 0 MariaDB skips;
  2 unrelated named-test skips; 13 packages have no tests; 25 process stages passed;
  CGO-free build passed; lint: 0 issues; source hashes unchanged.
Acceptance criteria still unverified: enabled watchdog paging/provider/mirror
  replay, commands/offline ACK, rotations/reset, bounded shutdown and actual
  fifteen-minute partition; final whole-M3 frontend/process gate.
```

## Production process check

Fresh app/probe/admin binaries passed all 25 stages of
`scripts/probe_runtime_smoke.py --verify-replay --verify-history` with a separate
fresh disposable MariaDB schema, two hub workers and local-only targets/providers.
The test starts workers before enrollment. The operator could enroll while a
worker owned the parent; the parent remained at epoch 1 while an edge restart
advanced the session generation from 3 to 4. Offline failure, provider retry,
process restart and recovery retained 21 telemetry records and replayed 16
observation sequences, one resolved incident and two successful delivery outcomes.
The hub created zero send intents. Final source and acknowledged sequence were
both 51. Production workers recomputed a closed minute with 32 source observations
and exactly 60,000,000 microseconds of overall coverage. All spawned processes
shut down successfully.

This process check ran the new controllers with disabled watchdog settings. It
proves integration and preservation of existing regional/replay behavior, not
enabled watchdog paging or the final fifteen-minute partition. The sanitized
report is `/private/tmp/phoenix-m3-watchdog-runtime-process-acceptance/report.json`.

## Retrospective and Antigravity feedback

Antigravity reviewed pasted source read-only in conversation
`f80be51b-d830-4ec7-84a9-c023909851f7`. It owned no files and invoked no repository
commands. The initial six findings were not accepted on authority alone:

- Both real repositories derive and persist `LastEnqueuedAt` in the delivery
  transaction. Adding a nonexistent record field would duplicate the contract.
- Runtime renewal preserves owner/epoch. Transactions reread persisted expiry;
  discarding the returned expiry snapshot does not bypass that fence.
- Keeping a failed tick before later admissions preserves event order. A genuine
  prolonged stall stops parent renewal; unenrolled ticks are explicitly discarded.
- ACK changes incident version without invalidating committed healthy continuity.
  Resetting every version change would incorrectly delay recovery after ACK.
- Production creates the runtime after acquiring its parent. Successful unarmed
  startup reads count as progress; true stalled persistence must not retain it.

The reviewer withdrew all six claims after receiving the missing adapter evidence
and named passing tests. Its statement that no bugs remain is an advisory review,
not proof of whole-system correctness or an executed acceptance gate. The coding
lesson is to trace the actual callee and lifecycle before generalizing from a
snapshot or missing field, then reproduce claims through observable effects.

Codex also made two test-construction mistakes: it initially added `StreamID` to
`ProbeConfigTarget` (which has no such field), and made degraded health fixtures
incoherent by keeping `ready=true` while setting DB/scheduler health false. The
compiler and real decoder rejected them. The fixtures now follow the actual
contracts, and the decoder is unchanged. This reinforces the same inspect-first
lesson for the integrator, not just the delegated reviewer.

## Remaining work

Explicit probe provider context, current-config/channel/incident reconciliation,
source-owner fencing at external I/O, watchdog mirror authorization and actual
both-side notifications remain required. Then complete durable commands/offline
ACK, rotation/reset, bounded shutdown, and the real fifteen-minute partition with
restart/history/recovery acceptance. The enabled guard stays until those watchdog
paths work. No frontend, Helm, schema or dependency changes are included here.
