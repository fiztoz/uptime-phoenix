# M8 — actual local image correspondence

Continuation after published image-content checkpoint `c983ec2`. This slice
closes the separately owed **local built-image correspondence** check. It is not
registry presence, publisher authenticity, reproducible-build proof, runtime
version, safe rootfs extraction, Docker loading or installation authority.

## Contract

Add `scripts/m8_image_correspondence_gate.py` and its stdlib unit tests. The
operator must explicitly allow public pulls and select a **local Unix Docker
endpoint**. The gate uses a private credential-free Docker config and the
built-in `docker` Buildx driver; it never starts a privileged builder container,
logs into a registry, pushes, loads, runs or deletes containers/images.

The gate builds a tracked-source snapshot from a recorded Git commit using the
actual `Dockerfile.probe`. Go/SQL/module changes or untracked Go/SQL files refuse.
An uncommitted Dockerfile fix requires explicit `--allow-working-dockerfile`;
the sole overlay and both tracked/working hashes are recorded. It never silently
verifies a stale Dockerfile while claiming to have tested the working fix. It resolves and hashes the public Go/distroless index
bytes, then changes **only the two base references** in the copied Dockerfile to
those immutable references. Original/rendered Dockerfile hashes are recorded.
Fresh local builds export OCI bytes and independent BuildKit metadata for
**each platform sequentially** to bound memory on small Docker VMs. Each original
index is verified independently; no combined registry index is fabricated or
claimed. The first combined fixed build exhausted local VM memory, not an
assertion; no global daemon/worker/memory settings were changed. No untracked files, task artifacts,
Docker credentials, SSH material or inherited application environment enter the
build context. Shared built-in builder caches are not globally pruned.

The exported archive is read with bounded stdlib tar handling: regular files and
canonical OCI directories only, unique names, no links/special files/traversal,
limited counts and sizes. Image layer tar bytes remain opaque blobs: no rootfs
entry is extracted. Every SHA-256-named blob is hashed before use. The trusted
index digest comes from the gate's own local build metadata, not an untrusted
checksum next to a downloaded image. This is local-builder trust, not independent
publisher authenticity.

For both Linux amd64 and arm64, resolve the exact original platform manifest and
config, then invoke the existing `verify-image` and `verify-image-content` CLIs.
Check their complete required selection fields and honest non-claims. Also read
the final COPY layer's one regular executable to a **generated private path**,
never a tar-provided path. Bound its entries/bytes, check ELF64 little-endian
`e_machine` against the platform, and reuse `verify-artifact` for static Go/probe
component checks. No executable is run and no merged rootfs is reconstructed.
Corrupt config/layer and wrong-binary-platform controls must fail with the intended
fixed errors, without success JSON. A binary digest sampled from this own build
is local correspondence evidence, not an independent release/publisher pin. Missing platform/control/CLI execution, malformed output, nonzero
build exit or resource-bound violation fails the gate; parent-only assertions
and skipped execution never count.

Temporary copied source and compiled host verifier live under dot-prefixed
subdirectories so Go package discovery cannot include them. Logs, pins,
commands, reports and exact versions are private artifacts; created artifacts
remain task-owned. Cleanup must never prune shared Docker caches or remove
pre-existing containers/images. No real MariaDB is needed for this stateless
verification; broader engine and operational gates remain open.

## Verification

The first real build exposed a genuine Dockerfile bug: both platform labels
contained the same amd64 ELF. The strengthened gate refuses that original export.
`ARG TARGETARCH=amd64` overwrote BuildKit's automatic target; redeclaring
`ARG TARGETARCH` inherits it. The Dockerfile fix removes defaults from both
automatic TARGETOS/TARGETARCH declarations. No published-image or production-host
impact is inferred without checking those artifacts. The actual fixed build and
all three CLIs passed for both architectures, as did the final full Go gate,
source/inventory/configuration/skip audit and owned-scratch cleanup.

Required: parser/source-snapshot/archive bounds and unsafe-entry tests,
command/flag/environment isolation tests, explicit refusal tests, actual
dual-platform local build with all three existing CLIs and negative controls,
and an unchanged-source audit. Existing content/metadata/binary controls
remain required. Network access for this local verification was explicitly
approved by the operator; no publication, deployment or remote SSH was approved.

## Repeatable commands

The quick offline regression suite is now part of `make release-image-gate`,
already called by the backend CI job and local `gate-full`. It requires neither
Docker nor network. Mocked CLI tests are plumbing controls, not image acceptance.

```sh
python3 -B -m unittest discover -s scripts -p test_m8_image_correspondence_gate.py -v
make release-image-gate m6-backend-harness-gate
```

For actual image verification, start the local Docker daemon and explicitly
approve public pulls. `GO_BIN` must point to the actual compiler matching the
Dockerfile's locked Go version, not a launcher that needs to download a toolchain.
The gate keeps host Go module/toolchain networking disabled. Image build steps
use approved public base/module downloads inside the local builder.

```sh
: "${GO_BIN:?Set GO_BIN to the installed compiler matching Dockerfile.probe}"
DOCKER_ENDPOINT="$(docker context inspect --format '{{.Endpoints.docker.Host}}')"
case "$DOCKER_ENDPOINT" in
  unix:///*) ;;
  *) echo 'A local Unix Docker endpoint is required' >&2; exit 1 ;;
esac
BUILDX_PLUGIN="$(docker --host "$DOCKER_ENDPOINT" info --format '{{range .ClientInfo.Plugins}}{{if eq .Name "buildx"}}{{.Path}}{{end}}{{end}}')"
test -n "$BUILDX_PLUGIN"
mkdir -p tmp
python3 -B scripts/m8_image_correspondence_gate.py \
  --allow-public-pulls \
  --docker-endpoint "$DOCKER_ENDPOINT" \
  --buildx-dir "$(dirname "$BUILDX_PLUGIN")" \
  --go "$GO_BIN" \
  --work-dir "tmp/m8-image-correspondence-$(date +%s)"
```

For an intentional **uncommitted Dockerfile-only** fix, append
`--allow-working-dockerfile`; the exact sole overlay is recorded. Other dirty or
untracked Go/SQL/module source refuses rather than claiming the tracked snapshot
contains it. After committing the fix, this flag is unnecessary. The work
directory must not exist; no prior artifact or optional execution is substituted.
Failure exits nonzero. Logs and outputs are private, bounded and task-owned.

## Validated correction: target labels versus executable bytes

**Symptom and repro.** The unmodified `c983ec2` Dockerfile produced an executable
of 45,686,946 bytes with SHA-256
`d15875047886dda947b464f83be16f51d3f2880efc5126519cd630ea1dae4ab6`
under **both** amd64 and arm64 image descriptors. Three independent bounded
read-only inspections of the final COPY layer found ELF machine 62 in both;
arm64 requires 183. No image or executable was run. The strengthened real gate
rejects the retained original export with `copied probe ELF architecture mismatch`.

**Mechanism.** `FROM --platform=$BUILDPLATFORM` correctly selects the native Go
builder. However, stage declarations `ARG TARGETOS=linux` and especially
`ARG TARGETARCH=amd64` supply defaults instead of inheriting automatic target
arguments. Both targets therefore executed `GOARCH=amd64`, even though BuildKit
correctly labeled the final image configs differently. The original compile log
records `[linux/arm64->amd64 go-builder] GOARCH=amd64`. Matching metadata/layer
hashes accurately described those bad bytes; they could not prove architecture.

**Falsification.** Identical totals might have been coincidence or a gate selection
bug. The two distinct original platform manifests contained the same binary hash,
ruling out coincidence. The recorded command selected both platforms with only
VERSION supplied. A tiny actual pinned-base BuildKit differential changed only
`ARG TARGETARCH=amd64` to `ARG TARGETARCH` for linux/arm64: observed value changed
from amd64 to arm64. No debugger could attach to the already-completed compiler;
source/recorded-command tracing and that one-knob experiment established the path.

**Fix.** Redeclare TARGETOS/TARGETARCH with **no defaults**. Retain native building,
`CGO_ENABLED=0`, pinned Go version, trimpath and all release/startup contracts.
The gate additionally checks actual copied ELF bytes, reuses the existing static
Go/probe verifier and demands wrong-platform refusal. This catches the cause,
not just differing image labels or layer sizes.

**Why it slipped through.** Correct direct Go cross-builds and synthetic metadata
fixtures do not exercise Dockerfile argument binding. Metadata/config validation
cannot see the executable architecture. The new default-CI offline regression
asserts unvalued automatic ARG declarations, while the separately approved real
build gate inspects each exported executable. No claim is made about any existing
published tag or production host without inspecting that artifact.

## Executed evidence and remaining acceptance

Private ignored evidence: `tmp/m8-built-image-verification/`. Final source hash,
including Dockerfile and both Python files:
`1cd1f836533118618f9006eec5554fe7cadccd65b75179f5f05da90eec32f429`
(1,480 source files); Makefile/docs are audited separately. This verified tree
was committed locally as `326ceda` after published `c983ec2`, with no commit-only
test rerun and no new push, image publication, deployment or opt-in. The private
`committed.json` binds that checkpoint to unchanged source/docs/config. Later
[private snapshot work](M8_IMAGE_SNAPSHOT.md) is a separate prerequisite; it does
not retroactively expand this built-image evidence.

- `run-01`: genuine original combined export passed its narrower metadata/content
  checks and four corrupt-byte controls. That was **not** correct ARM executable
  acceptance. Three `elf-repro` failures and `strengthened-red` prove the gap.
- `target-binding-red`: the new fast regression failed on both old ARG defaults;
  `unit-control-02` passed after the Dockerfile fix.
- `run-02`: combined fixed build failed `ResourceExhausted: cannot allocate memory`
  while compiling both targets. Kept as failure, not a compile/assertion success.
- Reviewer identified a real new-gate pin rewrite defect: a preceding comment
  containing a base tag intercepted whole-text replacement. `pin-comment-red`
  failed; substitutions now target only the matched FROM lines. A separate
  concern about parsing-before-digest was not the current adapter behavior;
  actual CLIs already compare digests first. Controls were nevertheless made
  shape-preserving: canonical diff-ID text in config and gzip MTIME only, so
  JSON/codec errors cannot mask the intended digest assertion.
- `run-03`: final source, **112.443 seconds**, successful sequential actual builds
  and **12 real CLI executions**: metadata, content, static binary, wrong binary
  platform, corrupt config and corrupt layer for each architecture. Six intended
  negative refusals; no skipped or substituted platform. Source is tracked c983ec2
  plus the explicit working Dockerfile overlay, not a falsely attributed commit.
- **24** offline unit tests, **six** intended assertion mutants, fresh untouched
  unit control, `make release-image-gate m6-backend-harness-gate`, **27** existing
  dual-engine parser and **17** shard regressions passed. Mutants target ELF
  machine, FROM-offset pins, archive paths, binary acknowledgements, automatic ARG
  defaults and required negative controls; no compile/timeout substitutes.
- Cached Go **1.26.9**: native `go build ./...` (**6.406 s**), `go vet ./...`
  (**0.707 s**) and artifact `-race -count=3` (**918 named passes**, zero
  failures/skips, **30.109 s**) passed on the frozen final source.
- `go test -race -count=1 -json -timeout 3600s -p 4 ./...`: **6,055 named passes**,
  **23 passing packages**, zero failures, **1,907.931 s**. All **547 named skips**
  accounted for: **542 missing-MariaDB-DSN** plus five known optional/helper cases;
  14 no-test packages, zero malformed events or unexplained skip reasons. This
  default run establishes **no new MariaDB acceptance**. Earlier interrupted
  pre-fix/review campaigns are retained and not final acceptance.
- `cleanup-result.json`: **23** identified private transient paths removed after
  joined users, frozen-source and exact ownership checks. Three raw OCI exports,
  original/fixed metadata, pinned Dockerfiles, repros, overlays and logs retained.
  Both baseline containers preserved. No Docker container/image/volume deleted;
  shared builder/base/module caches intentionally not pruned. No unknown external
  resource or earlier worker-backup cleanup is claimed.
- `final-audit.json` binds source contents/inventory, Makefile/unchanged-CI config
  and final documentation hashes. Local links, shell examples, Go formatting and
  whitespace pass; no task scratch/users remain. `golangci-lint`/`govulncheck` are
  unavailable: vet/format are fallback evidence, not completed lint/vulnerability
  scans. Frontend/Helm gates were not run; their sources are unchanged.

| Actual copied executable | amd64 | arm64 |
|---|---|---|
| ELF machine | 62 | 183 |
| Size | 45,686,946 | 42,729,634 |
| Layer count | 13 | 13 |
| SHA-256 | `d15875047886dda947b464f83be16f51d3f2880efc5126519cd630ea1dae4ab6` | `d8402bc1d4ec7d2ffda05901233fcb04a978e879b476e8dbd66e5f93b320a513` |

Resolved public base indices for this run:
- `golang:1.26.9-alpine@sha256:3082400e369fa24d5fc60bca20edab3f6d604e0c5a690ec66b295eff4dd87ade`
- `gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab`

Each correct platform has its own original build index; the final successful run
does **not** claim a combined multiarch export or a published registry index.
Image configs include inherited distroless configuration and passed the existing
startup contract. Runtime-version proof remains false: sampled binary digests
and config labels are not independent publisher pins or authenticated runtime
identity. No merged rootfs is reconstructed. Images are never loaded/run/pushed;
only approved build steps execute in the built-in local builder.

This adds no remote adapter, installer/API/startup wiring, DB schema or new Go
dependency. Actual registry pull/platform checks for delivered bytes, immutable
artifact delivery, host ownership/fencing/cleanup and joined termination,
authenticated enrollment plus exact config activation remain open, as do broad
M7 MariaDB and M6/M8 operational/release/deployment gates.
