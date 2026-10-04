# M4 source-owned escalation

A remote monitor with an effective escalation policy can now be published. The
source that owns the assignment runs the ladder. The hub mirrors the progress
and does not send those steps. This is the escalation bullet of
[M4](IMPLEMENTATION_PLAN.md). Group and status-page recovery, Insights,
backup/restore, and deployment documentation remain open.

## Behavior

Precedence is unchanged: direct monitor policy, then the nearest ancestor group.
A disabled or empty policy is still assigned and does not fall through. The
remote encoder used to reject an enabled policy with steps. It now publishes
that policy. The edge validates and runs it.

Step zero stays the direct monitor notification queued with the opening
observation. The policy starts at its first step, due `wait` after the incident
opened. Each due step is one transaction: the incident version advances, the
escalation object moves to the next rung or to `done`, and the step's channels
are queued on the source outbox. Provider I/O happens later in the existing
delivery worker. The rendered message is `ESCALATION step N (policy ID): …`.
The snapshot does not carry the policy's display name.

Acknowledgement and recovery cancel a pending ladder. A ladder that already
finished stays `done`. Cancelling does not invent provider work. Certificate,
capacity, and watchdog incidents still cannot carry escalation.

A firing incident may publish another firing transition when the only change is
ladder progress. The direct notification and an earlier rung stay sendable
while the incident is still firing. Acknowledgement and recovery leave that
status, which is what supersedes unsent availability work. If a maintenance
window is active at send time, the existing edge delivery worker supersedes
the queued send, the same way it treats a direct notification. The ladder step
is still consumed.

Public `include_ack_url` stays off on remote channels. Remote acknowledgement
is the existing authenticated hub command.

## Storage

Edge migration `014_availability_escalation` adds the ladder columns to
`edge_alerts` and the step identity to `edge_delivery_outbox`. The hub already
stored those incident fields from migration `038`. Downgrade of `014` refuses
while any ladder row or escalation delivery exists. Run it with writers stopped.

Triggers on `edge_alerts` reference the new columns as `NEW.<column>`. An
unqualified name is planned against the pre-alter table and then fails every
insert with `no such column`.

## Verification

Executed in this checkout:

- `gofmt -w` on the changed Go files, then `go vet` on
  `internal/core/domain`, `internal/core/services`, `internal/core/ports`,
  `internal/adapters/probe`, `internal/adapters/repository/edge`, and
  `internal/adapters/repository`.
- `go test -count=1 -timeout 300s ./internal/adapters/repository/edge/ ./internal/adapters/probe/ ./internal/core/services/ ./internal/core/domain/ ./internal/adapters/scheduler/`
  — 2329 passed.
- `go test -count=1 -run TestIncidentEscalationRoundTripAndDuplicateSnapshot ./internal/adapters/repository/`
  — passed. It checks the hub model round trip and that a resolved incident
  cannot keep a pending ladder.

The edge test opens a pending ladder, reopens the process, advances the only
step to `done`, stores the step intent, and refuses downgrade. The recording
test checks that a disabled path is not required for arming, that step zero is
still the direct link, and that recovery cancels the ladder. The authorizer
test accepts an opening and a later rung, and rejects a rung that does not move.

Not run: the compiled `scripts/probe_runtime_smoke.py` harness has no escalation
flag. MariaDB was not re-run for a new hub ingest matrix; the hub columns and
model mapping predate this slice, and the new check is the in-memory model
round trip plus the existing repository package compiling. `golangci-lint` was
not run. Local acceptance does not authorize push, deployment, or a production
migration.
