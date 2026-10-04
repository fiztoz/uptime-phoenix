# M3 completion acceptance

Date: 2026-09-21. M3 is complete for the supported HTTP/TCP/DNS edge runtime.
Codex verified the implementation and integration; Antigravity's reviews were
advisory. [Machine-readable evidence](M3_COMPLETION_EVIDENCE.json) records exact
source/binary/log hashes and executed requirements. No push or deployment.

## Requirement audit

| Requirement | Verified effect | Evidence |
|---|---|---|
| Complete configuration | Saved source survives API/worker split and restart; whole protected config activates atomically; durable receipts follow commit | [Config acceptance](M3_CONFIG_SYNC_ACCEPTANCE.md), `TestRemoteConfigSyncContract`, final process |
| Ordered replay and bounded storage | Contiguous receipts/cursors, permanent rejection, retry and declared gaps; metadata and physical admission bounds | [Replay](M3_REPLAY_ACCEPTANCE.md), [retention](M3_RETENTION_ACCEPTANCE.md), [storage](M3_STORAGE_BOUNDS_ACCEPTANCE.md), final gate |
| Current state before history | Snapshot application cannot advance replay cursor or create provider work; source sequence and missing-state barriers survive restart | [Current state](M3_CURRENT_STATE_ACCEPTANCE.md), real initial receipt before retained replay below |
| Historical recomputation | Assignment/time guards and dirty 1m/1h/1d/overall history, with restart and concurrent changes | [History](M3_HISTORY_ACCEPTANCE.md), `TestProbeHistoryAcceptance`, production worker process stage |
| Both connection watchdogs | Application-health arming, loss and stable recovery, durable source incident, restart without duplicate initial notification | [Watchdogs](M3_WATCHDOG_ACCEPTANCE.md), current real watchdog partition and repository contracts |
| Durable commands and recovery | Original-incident ACK idempotency, credential/certificate rotation and explicit authenticated stream reset | [Commands](M3_COMMAND_ACCEPTANCE.md), [credentials](M3_HUB_CREDENTIAL_ACCEPTANCE.md), [certificates](M3_HUB_CERTIFICATE_ACCEPTANCE.md), [reset](M3_HUB_RESET_ACCEPTANCE.md), final gate |
| Pressure and termination | Telemetry/metadata diagnostics; stop admission, join producers, bounded ACK drain, retain exact unacknowledged bytes | [Shutdown](M3_SHUTDOWN_ACCEPTANCE.md), [storage](M3_STORAGE_BOUNDS_ACCEPTANCE.md), final compiled termination checks |
| Actual fifteen-minute partition | DOWN and recovery occur while disconnected, with edge restart; exact retained replay, current state first and one original ACK | Current process run below |

## Current process evidence

Fresh CGO-free app/probe/admin binaries, two live hub workers, a private edge
SQLite store and a fresh disposable MariaDB schema passed 51 stages.
A transparent local TCP relay interrupted only management traffic; TLS remained
end to end. The actual command-era partition lasted 900.005
monotonic seconds. An independent target failed, survived edge restart and
recovered before reconnection; the original target retained its pending ACK.

The restarted source preserved 5 exact pre-stop
rows. All 1,440 retained events had exactly one matching
kind/digest receipt with no rejection; 1,434
observations retained their original microsecond timestamps. The initial current
state was UP at source sequence 1678, with snapshot
watermark 1678. Its durable application time
(1789953748429843 UTC epoch microseconds) preceded the
first retained replay receipt (1789953748473222). The hub created
zero provider intents for mirrored remote history.

The queued ACK produced one durable source receipt and one accepted ACK transition
for its original incident. A repeated command remained idempotent; a new ACK of
the resolved original returned `already_resolved` and did not silence a later
outage. Both watchdog owners also demonstrated disconnected DOWN, restart
continuity and stable UP. The report includes measured shutdown durations and retained
prefixes; no provider exactly-once claim is made.

## Final gate and boundaries

Final CGO-free build, full race suite and lint passed on the recorded source:
3,719 named passes, 22 packages, zero failures,
2 optional skips, 315 MariaDB-named cases,
and 303 audited live-engine cases with no engine
skips. Earlier [shutdown evidence](M3_SHUTDOWN_EVIDENCE.json) records frontend
check/build/lint, 251 unit tests, 12 Chromium journeys, Helm variants and vulnerability
checks. Those surfaces and dependencies are unchanged by this final increment.

The first full-duration run completed its partition but failed a later harness assertion that counted legitimate hub watchdog sends as mirrored work. The corrected full-duration rerun is the accepted result above.

The first two short rehearsals exposed an API-setup mistake and a real assignment
deadlock; both are retained in [the retrospective](../postmortems/2026-09-21-m3-integration.md#assignment-and-authority).
The third passed 42 stages over 30 seconds and was explicitly a rehearsal, not the
900-second acceptance. After correcting the combined watchdog ownership assertion, a further 30-second run passed all 51 combined stages before the final long run completed. The initial full-gate clock-fixture failure is also retained.
Antigravity's incorrect findings were checked against source and effects, corrected
and acknowledged; no delegated report substitutes for this gate.

M4 feature compatibility, auxiliary edge TLS/capacity state and M5 fleet UI remain
separate milestones. The WAL threshold is write admission plus one transaction,
not a hard filesystem quota. External provider acceptance can remain uncertain
across a crash; durable replay is distinct from provider exactly-once delivery.
Production rollout and populated operator-installation migration rehearsal are
not claimed by this local milestone.

```text
Commands executed: full CGO-free build; full Go race suite with TEST_MARIADB_DSN; golangci-lint; compiled --verify-replay --verify-history --verify-watchdog --verify-command --verify-partition --verify-shutdown --command-partition-seconds 900.
Engines and named tests exercised: real SQLite and MariaDB, TLS management, two workers, actual HTTP targets/webhook, required cases listed in M3_COMPLETION_EVIDENCE.json.
Passed / failed / skipped: 3719 final named passes; 51 final process stages; zero final failures; 2 optional skips; earlier failures retained separately.
Acceptance criteria still unverified: none for M3's supported runtime scope.
```
