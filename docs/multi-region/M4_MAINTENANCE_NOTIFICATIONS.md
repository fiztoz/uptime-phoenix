# M4 maintenance and notification sync

Accepted maintenance schedules, direct notification links, provider and template
settings, target visibility, tags, owner and the effective escalation policy
travel in the snapshot and are what the edge executes. This is the remaining
compatibility bullet of [M4](IMPLEMENTATION_PLAN.md).

## Behavior

- The hub expands persisted monitor links into assignment `maintenance_ids`.
  A window with no link to an assigned monitor is omitted. An extra window that
  is not named by the assignment does not suppress.
- Cron windows keep their IANA timezone and minute duration. `02:15` in
  `Asia/Bangkok` suppresses a `0 2 * * *` / 60-minute window; the same wall
  clock in UTC does not. The America/New_York spring-forward gap stays inside
  the accepted duration. An empty timezone is UTC. An unknown name fails the
  check instead of suppressing on the wrong clock.
- Evaluation reads the accepted snapshot and the clock. A second decode of the
  same bytes, the restart case, suppresses the same instant.
- Direct links keep `include_target`. Remote acknowledgement URLs stay off.
  Provider `config` and the template title/body survive activation. Group
  channels are not copied onto the assignment.
- Regional alerts use the effective owner, not the monitor's own contact, and
  fill `probe.name` / `probe.location` from the accepted display metadata.
- The effective escalation policy, including a disabled policy that stops
  inheritance, is the policy already resolved into the snapshot. The source
  runs that ladder; this slice does not reopen escalation execution.
- `cmd/app`, `cmd/worker` and `cmd/probe` import `time/tzdata`. The current
  `distroless/static` image also ships zoneinfo. Hiding that directory makes a
  binary without the import fail `LoadLocation`, while the embedded database
  still resolves `Asia/Bangkok` and the New York DST transition.

## Production changes

| File | Change |
|---|---|
| `cmd/app/main.go`, `cmd/worker/main.go`, `cmd/probe/main.go` | embed the IANA timezone database |
| `internal/core/services/edge_delivery_service.go` | regional alerts carry the accepted probe name and location |
| `Dockerfile`, `Dockerfile.split` | note that zone data is embedded, not copied from the base image |

## Verification

Executed in this checkout with `GOTOOLCHAIN=go1.26.6`:

- `go test -count=1 ./internal/adapters/scheduler/ ./internal/core/services/ ./cmd/probe/ ./cmd/app/ ./cmd/worker/ -run 'TestPublishedMaintenanceAndNotificationContextApplies|TestEdgeDeliveryServiceReconcilesAcceptedGraphAndRedactsErrors|TestEdgeRecordingMaintenance|TestCronEvaluator'` — 16 passed, 0 failed.
- `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/phoenix-probe-tz ./cmd/probe` contains `Asia/Bangkok`, `America/New_York` and `Pacific/Kanton`.
- Distroless `gcr.io/distroless/static-debian12:nonroot`, with `/usr/share/zoneinfo` hidden by an empty mount: a program that imports `time/tzdata` printed `Asia/Bangkok America/New_York 2026-03-08T04:00:00-04:00` and exited 0. The same program without the import exited 1 with `unknown time zone Asia/Bangkok`.

Not run: `go test -race ./...`, MariaDB matrix, `scripts/probe_runtime_smoke.py`. Local acceptance does not authorize push, deployment, or a production migration.
