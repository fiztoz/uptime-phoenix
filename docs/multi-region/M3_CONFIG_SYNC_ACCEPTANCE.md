# M3 increment: automatic configuration synchronization

> Historical checkpoint: implementation limits and remaining work below describe
> that checkpoint. Use [current status](IMPLEMENTATION_STATUS.md) and
> [final M3 acceptance](M3_COMPLETION_ACCEPTANCE.md) for today's scope.

2026-09-20, configuration-sync commit `311e50a` on `codex/multi-region-probe-plan`.
**Result: configuration-sync increment passed independent acceptance.**
This is the first M3 increment, not completion of telemetry replay or the milestone.
The changes are committed locally; nothing was pushed or deployed.

## Behavior

Normal `PROBES_ENABLED=true` workers now build remote configuration from saved
authorized assignments and dependencies. A normal API edit reaches an enrolled
probe without an operator constructing JSON. Source capture and protected desired
publication share a serializable transaction; unchanged source retains its exact
revision and ciphertext. Source edits survive missed hints and restarts because
the database is always the reconciliation input.

The existing 047 snapshots and 049 active-pointer/receipt tables hold durable
sync work. No migration, new dependency, provider, monitor type or frontend change
is needed. Local resolution/encoding behavior remains behind its existing wrappers.
Remote DTOs preserve source UUID, generations, paused work and disabled referenced
dependencies. Remote ACK links are false without mutating local preferences.
Unsupported capabilities fail visibly before desired publication.

Hub receipt persistence verifies the exact protected snapshot and current
unexpired connector owner/generation. A final receipt-insert failure rolls back
the pointer write. Repeated receipts preserve the first stored application time;
stale or expired callbacks cannot advance or regress application evidence.
Receipt storage failure and the 60-second missing-receipt deadline close the
connection for idempotent retry. Health and socket writes are not receipts.

`phoenix-probe-admin status` now returns `applied_revision` and `sync_pending`
alongside its existing prepared `revision` and credential `state`. Revisions stay
decimal strings. The updated operator guide describes automatic reconciliation
and the limited diagnostic role of manual preparation.

## Independent evidence

Commands use `rtk proxy`, `GOTOOLCHAIN=go1.26.6` and
`GOCACHE=/private/tmp/phoenix-go-cache-template-race`.

| Check | Result |
|---|---|
| M2 runtime/session/connector audit rerun with race detection | PASS; initial sandbox listener denial was rerun with permitted local socket access |
| New remote encoder and existing local encoder race tests | PASS; exact remote identity, deterministic bytes, dependency revisions, local ACK preservation, unsupported-work rejection |
| Real TLS transport race test | PASS; exact application callback, redacted receipt-write failure, bounded stalled receipt commit and joined shutdown |
| New remote sync SQLite and live MariaDB contracts | PASS; edits/restart/no-op, concurrent publication, removal, wrong key, disabled probe, lease fencing, late rollback, immutable retries |
| Affected live DB regression matrix | PASS (49.461s); `/private/tmp/phoenix-m3-sync-live-db.log` |
| Scoped Go lint before final gate | PASS, 0 issues |
| CGO-free app/probe/admin builds | PASS; `/private/tmp/phoenix-m3-bin/` |
| Actual two-worker API/edge process smoke | PASS; `/private/tmp/phoenix-m3-sync-process/report.json` |
| Full `make gate-full` | PASS, exit 0; `/private/tmp/phoenix-m3-sync-gate-full.log`: Go build/vet/full race, 0 lint issues, frontend check/unit/build/lint, 12 Chromium tests, Helm matrix and diff check |

The full gate reported zero called-symbol/imported-package vulnerabilities; three
required modules had findings outside called code. It also reported zero Svelte
errors and warnings.

The live DB run explicitly used disposable `phoenix_m3_sync_ci` on local
MariaDB port 43316. It ran `TestRemoteConfigSyncContract`, `TestLocalConfigSourceContract`,
`TestLocalDeliveryContract`, `TestProbeConnector*` and `TestProbeConnection*` with
`-race -count=1 -p 1`; MariaDB was not skipped in this matrix.

The process test used fresh `phoenix_m3_sync_smoke` and the real operator commands.
It observed initial automatic revision 1, a saved API name edit producing revision
2, and a durable hub receipt for both. Two workers retained one management session
across a renewal. All hubs then stopped; a provider 503 left durable work, the
edge restarted offline, and provider recovery produced exactly one successful DOWN
and UP for the same incident. Final stream stayed unchanged, source sequence
advanced **27 → 47**, connection generation **3 → 4**, and config remained **2**.
The script stopped all child processes after success.

## Commit verification

The user subsequently requested proper commits. The work was split into M1 fixes
(`18762e7`), M2 runtime (`32c26a8`) and this M3 increment (`311e50a`). The final
indexed implementation was checked byte-for-byte against the source that passed
the full gate and live process checks above. No source changed during the split.

The isolated M1 staged tree also passed a CGO-free build and race-enabled local
delivery, escalation and lock-order contracts. The isolated M2 tree passed its
CGO-free build and focused identity, transport, scheduler, edge storage, service,
credential and connector race tests. Initial sandbox loopback-listener denials
were rerun with permitted local socket access and passed. These focused checks
verify the intermediate commit boundaries; they do not replace the full gate.

## Ownership, review and remaining work

Antigravity's completed M2 audit was checked independently; see
[the disposition](../postmortems/2026-09-21-m3-integration.md#review-practice-and-follow-up). Native computer control
could not deliver a new M3 assignment. Codex updated the ownership contract before
taking over all implementation files. No M3 implementation is attributed to
Antigravity. A delayed response may edit only its separate review document.

Reconciliation is asynchronous (normally the 15-second renewal interval plus
reconnect/backoff); it cannot revoke unreachable work instantly. Existing accepted
configuration remains the edge authority during a partition. The supported runtime
is still HTTP/TCP/DNS and direct delivery. Telemetry replay/ACK, retention/gaps,
current-state restoration, historical-generation authorization, watchdogs and
commands remain unfinished. Hub health still reports ingestion unavailable.

Next bounded increment: connect durable ordered edge telemetry to fenced hub
ingestion. Require transactional contiguous cursor advancement, explicit rejection
and retry receipts, lost-ACK replay idempotency, historical assignment authorization,
and zero provider sends during mirroring. Do not wire replay through local
`HeartbeatService.Record` or delete source evidence based on health diagnostics.
