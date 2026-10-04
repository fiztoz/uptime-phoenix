# M4 group and status-page recovery

Folder alerts and public status pages now follow overall policy for a monitor
that is assigned to a remote probe. A local recovery does not close the folder
or a public incident while another region is still down, or while evidence is
unknown. This is the group/status-page bullet of
[M4](IMPLEMENTATION_PLAN.md). Insights, backup/restore, and deployment
documentation remain open.

## Behavior

Local-only monitors are unchanged. Their latest heartbeat is still the status
used by folder rollup, the public page, the SVG badge, and incident recovery.

A monitor with any non-local assignment uses `EvaluateMonitorHealth`. `any_down`
stays down while one fresh region is down. `all_down` is up when one region is
a fresh up. Stale, missing, or future evidence is `UNKNOWN`. `UNKNOWN` and
`PENDING` do not send a recovery and do not clear a confirmed folder DOWN.
The next fresh overall UP still recovers that folder, because the stored
incident stays DOWN through the gap.

Status-page auto-resolve uses the same gate. It also runs on a later local
check that is not itself a down-to-up transition, once overall policy is up.
A failed overall read does not resolve.

Public monitor status, the uptime-bar day, and both chart responses keep
`unknown` as its own value. Unknown time is excluded from the uptime
percentage. The admin badge, group pill, and status filter do the same.
A remote assignment's browser heartbeat carries `overall_status`, and that is
what the monitor pill and folder rollup use.

Group `all_down` still reports up when one child monitor is a fresh up, even
if another child is unknown. `worst_of_children` and `threshold` stay unknown
until that evidence is fresh, and a down child still trips them.

## Verification

Executed in this checkout:

- `gofmt -w` on the changed Go files.
- `go test -count=1 -timeout 180s ./internal/core/domain/ ./internal/core/services/ ./internal/adapters/ws/ ./internal/adapters/http/handlers/ ./internal/bootstrap/`
  — 1042 passed.
- `cd web && bun test src/lib/monitor-filter.test.ts src/lib/status-page-health.test.ts`
  — 59 passed.
- `cd web && bun run check` — zero errors.

The new service tests cover mixed-region `any_down` staying down, stale
evidence staying unknown, `all_down` recovering on one fresh up, a folder
holding its incident through unknown, and status-page auto-resolve refusing
down and unknown. The chart test keeps an unknown interval out of downtime.

Not run: MariaDB, `scripts/probe_runtime_smoke.py`, and `golangci-lint`.
`bun run build` was not run. Local acceptance does not authorize push,
deployment, or a production migration.
