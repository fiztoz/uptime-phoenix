# M8 — probe activation (init, register, start, enroll)

Continuation after `8eac9f6`. An SSH install stops at `resources_ready`: files in
place and the unit enabled, not started. Activation takes one such install to an
**enrolled, running probe**. It is a separate durable operation, so the accepted
install journal (migrations 084–086, real-MariaDB accepted) stays unchanged.

## Decisions

- **Token path.** `probe init` prints a ten-minute, one-time enrollment token. The
  authorized helper returns it once, inside its receipt, over the pinned and
  authenticated SSH channel. The hub keeps it only in memory and passes it straight
  to the existing `ProbeAdminService.EnrollSource`. It is never stored, logged or
  returned by any API. If the hub loses it (crash between init and enrollment), the
  activation ends `interrupted`. Recovery (`probe token` reissue) is a later slice.
- **Reuse, not new trust.**
  - Host steps use the existing host-action grant: a separate one-time admission
    table, the same Ed25519 authority and nonce challenge, and the same helper verb
    and dispatcher.
  - Hub steps use the existing `CreateRegistration` and `EnrollSource`.
- **Step model.** The activation reuses `ProbeInstallResource` steps and the
  journal's causal rules (`validProbeStepGraph`), with the success state `active`.

## Layout (`ProbeActivationIdentity.Steps()`)

| # | Kind | Where | Reference | Effect |
|---|---|---|---|---|
| 1 | `probe_init` | host (authorized) | install data dir | `<artifact>/<release> init --data-dir <data dir>` as root, empty env; requires an empty data dir; returns `probe_id`, `stream_id`, `certificate_fingerprint` + token |
| 2 | `registration` | hub | endpoint | `CreateRegistration{ProbeID, Key, Name, Location, Endpoint, TLSPin=certificate_fingerprint}` |
| 3 | `service_start` | host (authorized) | unit name | `systemctl start <unit>`; applied only when `is-active` reports `active` within 20 s |
| 4 | `enrollment` | hub | endpoint | `EnrollSource(probeID, token, streamID)`; applied only when the operation succeeds |

The endpoint is `wss://<install SSH host>:<listen_port>/ws/probe/v1`, with
`listen_port` defaulting to 8443. The probe must be reachable from the hub there.

## Storage — migration `087_probe_install_activations` (both engines)

- **`probe_install_activations`** (one row per install `operation_id`, FK to
  `probe_install_operations`, RESTRICT):
  - identity columns: `probe_key`, `name`, `location`, `listen_port`,
    `client_key_fingerprint`, `requested_by`;
  - lifecycle columns: `owner_id`, `state`, `error_code`, `created_at`, `updated_at`,
    `deadline_at`;
  - facts: nullable `probe_id`, `stream_id`, `certificate_fingerprint`, set only
    together with `probe_init` applied.
- **`probe_install_activation_steps`**: per `(operation_id, kind)` the ordinal,
  reference, owned, state, unique attempt ID, error code and started/finished times.
  Same checks as `probe_install_resources`, with kind IN the four activation kinds.
- **`probe_activation_admissions`**: the shape of `probe_host_action_admissions`, with
  kind IN `probe_init`, `service_start` and an FK to the activation steps.
- **Down migration:** refuses while any activation or admission exists (the 085
  guard pattern).

The store (`repository.ProbeActivationStore`) implements `ports.ProbeActivationRepository`,
`ProbeActivationStepJournal` and `ProbeActivationAdmissionRepository`. It mirrors
`probe_install_journal.go` and `probe_host_action_admission.go`:
- the same transaction helper, DB-clock sampling after the authority lock, and
  expiry reconciliation;
- immutable exact retries and plain-INSERT admissions;
- UTC microsecond times;
- id tie-breaks wherever order matters.

## Host side

- `ProbeHostActionIntent` accepts `probe_init` and `service_start`. Their specs equal
  the activation layout's, and their marker and grant are the host-action grant's.
- The helper verb `stage-host-action-authorized` applies them:
  - **`probe_init`:** runs the staged binary (a protected path under the fixed
    `/opt` parent, mode 0555, root-owned) with fixed arguments, a 30 s timeout and
    4 KiB bounded output. Its stdout is parsed strictly.
  - **`service_start`:** fixed `systemctl start` and `is-active` polling.
- The `probe_init` receipt adds the facts and the token:
  `{"state":"probe_initialized","kind":"probe_init","marker_sha256":"…","probe_id":"…","stream_id":"…","certificate_fingerprint":"…","enrollment_token":"…"}`.
  The receipt is the only place the token appears; the helper never writes it to stderr.

## Hub side

- **`ProbeActivationRunner`** works like the install runner: begin, then reserve,
  dispatch, record and finish for each step, with a cancellation watcher. Host steps
  use `SendHostAction` / `SendProbeInit`; hub steps call the admin service. The token
  lives only in the runner's stack between steps 1 and 4.
- **`ProbeActivationOperationService`** works like the install API service: an
  API-local transient key, bounded capacity, joined Close.
- **Routes:**
  - `POST /api/probe-installs/:operation_id/activate`, body `private_key`,
    `probe_key`, `name`, `location`, `listen_port`;
  - `GET /api/probe-installs/:operation_id/activation`;
  - `POST /api/probe-installs/:operation_id/activation/cancel`.
  - Views list the steps and, once known, `probe_id` (needed to manage the probe).
    They never include the token, key, address or fingerprints.

## Implementation notes

- **Shared code.** The install journal's step validation was factored, unchanged,
  into `validProbeStepGraph`, which both `ProbeInstallOperation.Valid` and
  `ProbeActivation.Valid` use.
- **Activation host steps reuse the host-action grant.**
  - `NewProbeActivationActionAuthorizationService` adapts the activation admission
    table to the same authorization service. The install table from migration 086
    cannot admit `probe_init` or `service_start`.
  - The helper's `apply` now returns an optional receipt. Only `probe_init` returns
    one, and it is the only carrier of the token.
  - `SendHostAction` refuses `probe_init`; `SendProbeInit` returns the parsed result.
- **Token custody.** `ProbeActivationExecutor` holds the token from `probe_init`
  until the `enrollment` call, then clears it whatever the outcome.
  `ProbeActivationOperationService` also calls `Forget` when a run ends. The runner
  records public facts only.
- **Outcome mapping.**
  - `registration`: invalid, conflict and not-found errors are a definite
    `not_started`; anything else is `unknown`.
  - `enrollment`: succeeded is `applied`; a pre-write refusal is `not_started`;
    a failed or ambiguous exchange is `unknown`, because the token may have been
    consumed.
- **`active` is terminal.** The runner returns as soon as the last step records
  `active`, and a cancel of an `active` activation returns it unchanged.
- **Probe identity checks.** The `probe_init` action runs the staged binary only
  after verifying its size and SHA-256 against the pin through no-follow descriptors,
  and only on an empty 0700 data directory. Output larger than 4 KiB, or with
  unknown members, is refused. A non-empty `hub_id` means the probe is already
  bound, so it is refused.

## Operational limits

- **Activation window.** `resources_ready` is non-terminal in the install journal,
  which hard-expires an install 15 minutes after it was created. Activation must be
  submitted within that window; afterwards the install reconciles to
  `interrupted/deadline_expired` and activation is refused with 409. The client
  should call `activate` as soon as the install reports `resources_ready`.
- **Isolation from the install afterwards.** Once an activation exists, later install
  journal changes (cancel intent, terminal outcome, expiry) do not stop it. Only
  creation requires `resources_ready`.
- **Uniqueness scope.** Attempt IDs and nonces are unique within each admission table,
  not across tables. The marker binds the kind, and the signing domain separates
  host-action grants from staging grants.
- **Cancel during the last step.** If enrollment succeeds while cancel intent was
  recorded, the activation still ends `active`: the probe is enrolled, and calling it
  canceled would misdescribe it.

## Verification (local, SQLite)

| Area | Result |
|---|---|
| Store (migration 087) | 80 SQLite leaves pass; 80 MariaDB twins skipped (no DSN). They are required by the `m8-stage-authorization` gate scope, which now requires 215 MariaDB leaves. 8 of 9 store mutants caught; the survivor is unreachable |
| Runner | 92 tests (×3 without flakes); 3 deliberate breaks caught |
| Executor and API service | token custody, outcome mapping, dispatch-once, cancel and join, all race-clean |
| Handler | 451 tests; responses never contain the key, token, address, fingerprints, stream or owner IDs |
| Provisioning (receipt, `probe_init`, `service_start`, `SendProbeInit`, session) | 63 top-level tests, about 38 overlay mutants caught (3 redundant survivors). One parser defect (stray `}` after the object) found and fixed; two hardening items applied (session kind pin, artifact directory owner/mode check) |

**Full native race suite** (Darwin/arm64, no DSN, `-p 4`): 10,110 passes, 717 skips
(the previous 637 plus the 80 new MariaDB activation leaves) and **one failure**:
`TestProbeCommandStorage/sqlite/ExpiredRequestStillRetries`. This is an untouched M3/M7
command-storage test with a 3 s expiry, a 1 s lease and 20 ms slack; it fails if a loaded
run delays its first claim by about 2 s. It then passed 10 of 10 isolated race runs, and
a full rerun of the repository package passed (1,103 passes, 0 failures, 712 MariaDB
skips). No file in this slice touches command storage. It is recorded as a pre-existing
load-timing flake, not fixed here.

Not executed: real MariaDB, a real `probe init`/`probe run`, real systemd start and
real hub↔probe enrollment. These are the planned AWS acceptance for this slice.
