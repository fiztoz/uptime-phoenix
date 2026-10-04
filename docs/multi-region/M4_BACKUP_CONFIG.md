# M4 backup/restore and config-as-code with probe identities

Backup export/import and declarative config-as-code now carry remote probe
identities and monitor vantage-point sets. This is the "config-as-code and
backup/restore with stable probe keys and assignments" bullet of
[M4](IMPLEMENTATION_PLAN.md). Deployment documentation and the
clear-history/tombstone/stream-retirement definition remain open.

## Backup document (version 2)

`BackupDocumentVersion` is now 2; import still accepts version 1, whose
documents simply have no probe sections and leave every monitor on the legacy
local assignment. Two sections were added:

- `probes` — one entry per remote registration referenced by an exported
  monitor: `id`, `key`, `name`, `location`, `kind`. This is identity metadata
  and nothing else. Runtime session credentials, sealed credentials, connector
  leases, endpoint/TLS pin material and edge queues are deliberately not part of
  a backup and are never recreated on import.
- `monitor_probe_assignments` — one complete set per exported monitor:
  `monitor_id`, `health_policy`, and `members` of `probe_key` plus optional
  `binding_key`/`binding_kind` (probe-local resource references, never the
  bound endpoint). The reserved `local` key is a valid member and needs no
  `probes` entry; the reserved local registration itself is never exported.

Export only travels probes referenced by the exporting user's monitors; the
fleet list is install-wide and is not leaked through a per-user export.

## Import rules

**Identity.** The stable key is the identity. A document probe is resolved by
id first, then by key: an existing registration is reused (never overwritten),
a conflicting pair (the id exists under another key) is refused, and only a
truly unknown identity is created. Restored identities are created **disabled
and credentialless — restored remote identities remain disabled pending
reenrollment**. A restore therefore can never mint a duplicate live probe
identity; the summary reports every identity as `probes[]` with `reused` and
the counts `probes_created` / `probes_reused`.

**Reenrollment.** `phoenix-probe-admin register` is the documented reenrollment
entry point: it adopts a disabled registration whose key/name/location match,
re-enables it (revision-checked), and prepares the runtime credential as
before. Mismatched metadata still fails; a new identity is still created
enabled.

**Assignments.** A document set is authoritative for its monitor and is
committed through the new declarative assignment write (see below). When it
cannot be honored exactly the monitor is **not imported at all**:

- the probe stores are not wired on the target install → skip with
  "refusing to import it as local";
- a member key cannot be resolved (or the set has duplicates, an unsupported
  policy, or an invalid binding) → skip with the same refusal;
- the set commit fails after the monitor row was created → the row is removed
  again (compensating delete) and reported as "monitor was not imported".

A remote-only set therefore leaves the monitor remote-only and inert until its
probe is reenrolled; it is never silently rerouted to the hub scheduler.
`monitor_probe_sets_restored` counts honored sets.

## Config-as-code

The document schema (`phoenix.dev/v1`) gained two declarations:

- `spec.probes[]` — `key` (the stable probe key, a lowercase slug, never
  `local`), `name`, `location`, optional `enabled`. Probe identity is keyed
  natively in `probes.probe_key` and is **not** recorded in `config_keys`;
  resource ids are numeric and probes are UUIDs. A created registration
  defaults to `enabled: true` (nothing executes without an enrolled
  credential); an omitted `enabled` on update keeps the current state.
- `monitors[].probe_assignments[]` plus `health_policy` — the complete desired
  set by probe key (`local` or a declared probe) with optional binding
  references. An **omitted** list is unmanaged: the live set is left exactly as
  it is, so a document can never silently reroute a regional monitor back to
  local execution.

`validate` mirrors the assignment store's contract: unknown/duplicate probe
refs, unsupported policies, bindings on non-docker monitors or on `local`, a
remote Docker member without a binding, and remote `push` members all fail
validation. `plan` reports `probe` and `probe_assignment` changes and refuses
(marks the plan invalid) any set whose member would be disabled after apply —
including an existing registration whose `enabled` the document leaves
untouched. `apply` re-checks live registration state before committing a set
(defense in depth). Sets are committed as one complete desired set with
optimistic revision, so repeated apply of the same document is a no-op and an
exported document is a fixed point. **Prune never deletes probe
registrations** — they are retained identities with no delete path.

Connection material stays out of the document entirely ("connection secrets
stay secret references"): neither export path emits endpoint, TLS pin,
credential, token or queue data, and neither import path recreates any.

## Declarative assignment write

`ports.MonitorProbeAssignmentRepository` gained `Restore`, the one documented
difference from `Replace`: members must be **registered** but need not be
**enabled**, because a restored or just-declared identity is disabled or
unenrolled until an operator registers and enrolls it. Everything else is
identical — complete sets, optimistic revision, tombstoned removals, retained
generations, explicit resource bindings, and history commits. Live operator
input keeps using `Replace`, which still refuses a disabled registration.
`ProbeRegistryRepository` gained `GetByKey`, and `Create` allocates a canonical
UUID when the caller omits the identity so core code never mints UUIDs.

## Verification

Executed in this checkout:

- `go build ./...` and `go vet ./internal/...` — clean.
- `gofmt -w` on the changed Go files; `gofmt -l internal/` empty.
- `go test -count=1 ./internal/... ./cmd/...` — all packages ok (~1,200 tests).
- `go test -race -count=1 -timeout 2400s ./...` with `TEST_MARIADB_DSN`
  pointing at the disposable `phoenix-m01-fix-validation` MariaDB 11.8.9
  container — all packages ok, zero failures, zero skips of MariaDB matrix
  cases.
- Targeted race run with named passes on **both engines**:
  `TestProbeAssignmentRestore_AcceptsDisabledRegistration/{sqlite,mariadb}`,
  `TestProbeAssignmentRestore_KeepsDockerBindingContract/{sqlite,mariadb}`,
  `TestProbeRegistryCreate_AllocatesIdentity/{sqlite,mariadb}` plus the
  existing `TestProbeRegistryContract/{sqlite,mariadb}` and
  `TestMonitorCreateInitializesLocalAssignment/{sqlite,mariadb}` — all passed
  under `-race`.
- Service tests (mocked ports): export/import round trip with probes and sets,
  remote-only sets staying off the hub scheduler, unresolvable probes skipping
  the monitor instead of rerouting it, missing probe stores refusing assigned
  monitors, compensating delete when a set commit fails, identity reuse and
  conflict refusal, v1 compatibility and version rejection, wire-shape guard
  asserting the probe/assignment JSON carries no credential/token/queue/
  endpoint fields; config validate/plan/apply idempotence including the
  export→apply fixed point, disabled-member refusal (declared and existing),
  prune-never-deletes-probes, and probe metadata/enabled updates; CLI
  registration adopting a restored disabled identity and refusing mismatches.
  40 tests across `services` and `bootstrap`, plus 17 config/backup suites.

Not run: `scripts/probe_runtime_smoke.py` (no probe-restore flag), Playwright
E2E, `bun run build`/`bun run check` (no frontend files changed), and a
browser pass. `golangci-lint` reports zero findings on the changed files;
thirteen pre-existing findings in untouched files (see the commit message)
predate this slice. Local acceptance does not authorize push, deployment, or a
production migration.
