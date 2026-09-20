# M3 retention and replay recovery increment

Date: 2026-09-20. Builds on `eb5c59e`. M3 remains in progress.

## Behavior

The production probe retains up to 512 MiB of accounted telemetry for seven days by default. `PROBE_TELEMETRY_MAX_BYTES` (1 MiB–1 TiB) and `PROBE_TELEMETRY_RETENTION_HOURS` (1 hour–365 days) configure that policy. Accounted bytes include payloads and conservative per-row metadata; these values do not claim to cap the entire SQLite database or WAL file. Legacy low-level store callers without a configured policy retain the previous fail-closed bound.

Age and byte eviction persist loss ranges in the same transaction as row deletion. Repeated ordinary observations are evicted before incident transitions and outcomes. Ordinary writes leave at least 10% for critical evidence, and provider leases reserve their maximum outcome bytes before any send is authorized. Small queue budgets limit the number of new delivery leases so a large claim request cannot starve every provider. A bounded sweep commits eviction progress without authorizing provider work until capacity is available.

Loss metadata is capped at 1,024 ranges. If fragmentation exceeds that bound, the intervening oldest span is explicitly declared lost under `disk_pressure`. Recording, retention, partial acknowledgement, cursor movement and restart preserve the remaining prefix. Queue diagnostics expose pressure at 80% and retained gap ranges.

The edge sends `telemetry.gap` through the normal ordered replay pump. Hub migration 055 persists gaps, source observation-time bounds, affected assignment scope and pending range-recomputation work. The gap and canonical cursor commit under the existing database lease, installation and stream fences. A gap never deletes accepted history. Late replay inside a committed gap receives the permanent `history_gap` result rather than resurrecting evidence. A successful gap ACK contains zero accepted, duplicate or rejected event counts.

Recoverable ingest storage failures emit `telemetry.retry` only when a fresh durable cursor can be read within the sent range. An unreadable cursor, restored cursor behind the request, invalid identity or expired authority closes the connection. A retry frame never authorizes pruning.

## Verification ledger

- Edge race tests pass for age eviction, restart, byte pressure, retained critical events, stale ACK fencing, gap insertion rollback, partial ACK rollback, delayed ACK after coalescing, fragmented metadata and small-budget provider outcome reservations.
- Real SQLite and MariaDB gap tests pass for scoped gaps, duplicate prefixes, historical overlap, rejected resurrection, downgrade refusal and final cursor-write rollback (4.310s focused rerun).
- Real TLS WebSocket tests pass for callback mapping, zero-count gap ACK, bounded retry followed by success in the same session, invalid retry cursor and authority failure (1.912s final focused run).
- CGO-free `go build ./...` passed. `golangci-lint run` passed with zero issues.
- Full `go test -race -count=1 -timeout=20m ./...` passed; the longest handler package took 365.176s and repository tests took 191.330s. Log: `/private/tmp/phoenix-m3-retention-race-final.log`.
- Complete repository race matrix with live MariaDB and SQLite passed (194.982s), using `TEST_MARIADB_DSN=phoenix:phoenix@tcp(127.0.0.1:43316)/phoenix_m3_replay_ci?parseTime=true&loc=UTC&multiStatements=true`. Log: `/private/tmp/phoenix-m3-retention-mariadb-final.log`.
- No frontend or chart code changed in this increment. The full product/E2E gate will run again for final M3 acceptance.

The initial hub gap insertion failed on both engines because Bun inferred `affected_monitor_i_ds`; explicit `bun:"affected_monitor_ids"` mapping fixed it. A test-only query hook isolated the generated SQL and was removed. Antigravity's extra authority test initially rejected queued config frames sent before the authority callback; the corrected test asserts that no successful replay receipt or ready hub health escapes the failure. No production authority check was weakened.

The first full race run was interrupted by Codex for diagnosis after a long quiet handler test. The stack showed bcrypt password verification, not a deadlock. That interrupted run is not a pass; the complete longer-timeout rerun passed. The broad MariaDB run failed only in the existing installation downgrade test because Codex omitted the documented `multiStatements=true` DSN parameter. The reproduced SQL syntax failure was a test invocation defect; the corrected complete matrix passed. No source change was made to mask it.

## Scope remaining in M3

Gap-derived UNKNOWN coverage and recomputation are persisted work, not yet consumed. Full SQLite history/receipt/config cleanup and physical file reclamation remain open. Current-state snapshots, both connection watchdogs, durable commands/offline ACK, credential/certificate rotation, explicit stream reset and bounded shutdown flushing remain open. The final M3 acceptance still requires the real fifteen-minute partition, target DOWN/UP, edge restart, fresh state ahead of backlog, exactly-once retained replay and incident-specific pending ACK.

No new dependency, monitor type, notification provider, frontend change, push or deployment is part of this increment.

## Lessons for the implementing agent

Follow the source-to-effect path before declaring a milestone complete. A parser and a stored dirty marker do not prove a running synchronizer or corrected historical coverage. For each capability, name its runtime caller, durable transaction, observable effect and failure/restart test.

Use the exact database column and wire field names. The new gap model's initial `AffectedMonitorIDs` mapping failed deterministically on both databases; a generated-query trace established the mechanism, and the explicit Bun tag fixed it without changing the schema.

Derive assertions from guaranteed behavior. A failed authority callback must forbid successful receipts and readiness, but cannot recall config frames already queued before that callback. The transport regression verifies that effect while keeping the connection-close requirement.

Keep file ownership in force even inside workflow macros. The audit's `/learn` step edited `AGENTS.md` outside its assignment; that addition was identified and reverted. Subsequent review disabled slash-command expansion and used read-only mode. No project rules or agent permissions were changed to force a delegation through.

Audit the integrator too. Codex initially stopped a slow bcrypt test and omitted a documented MariaDB DSN option. Neither was a production failure, and neither run counts as a passed gate. Codex also initially rejected the review's backward-clock finding; the actual architecture contract proved it correct. The next state increment must preserve sequence ordering across clock correction and test it directly.
