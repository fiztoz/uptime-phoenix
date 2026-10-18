# M8 — digest-bound Docker image metadata

This checkpoint is committed locally as `54fdc44` on `feat/m8-ssh-preflight`;
not pushed. Publication is not deployment approval.

Continuation after `2155ef1`. This prerequisite resolves a trusted multi-platform
release index to one immutable platform manifest. It is **metadata verification**,
not image download, layer verification, publisher authentication or installation.
The existing broad M7 MariaDB acceptance blocker remains open.

## Frozen contract

- An explicit pin contains a canonical, fully qualified repository (no scheme,
  credentials, tag, digest, query or fragment), version without `v` (max 64),
  Linux architecture `amd64`/`arm64`, trusted index SHA-256 and exact index size.
- Three independently supplied local streams contain index, selected manifest and
  image config. Every blob is bounded to 4 MiB. Ownership of all three streams
  transfers on entry, including invalid input; close must unblock pending reads.
- Capture each stream once, check context throughout reading/parsing, and close
  all streams on cancellation/failure. No raw config, environment, history,
  annotations, filesystem paths or underlying diagnostics cross the boundary.
- Accept OCI image index/manifest/config and Docker schema-2 equivalents. Require
  schema version 2, exact descriptor digests/sizes/media types, and one unambiguous
  Linux target. Only baseline amd64 (no variant) and arm64 (empty/v8) are supported.
  Valid non-target platform descriptors and BuildKit unknown/unknown attestation
  descriptors do not become executable candidates; duplicate target claims refuse.
- The selected descriptor must bind the supplied manifest bytes; its config
  descriptor must bind the supplied config bytes. Refuse descriptor URLs, embedded
  data and artifact/subject payloads on the selected executable chain. Layer
  descriptors must have supported non-foreign media, canonical SHA-256 and positive
  size (at most 16 GiB per declared layer); config rootfs must be layers with
  matching diff-ID count. Indexes permit at most 256 descriptors and manifests
  at most 128 layers. Layer content itself is deliberately **not** read or
  claimed verified.
- Config OS/architecture/variant must match selection. Require the checked-in
  `Dockerfile.probe` contract: entrypoint `["/uptime-phoenix-probe"]`, empty default
  Cmd, user `65532`, and exact OCI title/source/version labels. Refuse healthcheck
  hooks, nonempty OnBuild instructions and implicit anonymous volumes. The version
  claim is `digest_bound_config_label`, not independent runtime-version evidence.
- JSON refuses duplicate decoded keys, case aliases of interpreted fields,
  non-object/trailing data, invalid UTF-8, depth beyond 32 and more than 65,536
  parser tokens.
  Bounded unknown extension fields may be ignored, never executed or returned.
- Return only the original pin, selected manifest/config digests and layer count.
  Derive `repository@sha256:<platform-manifest>` without tags or implicit registry
  normalization. A future installer must persist parent and child identity, pull
  by that digest with explicit platform, independently verify daemon results and
  remote bytes/resources, then require authenticated enrollment and exact config
  activation. A metadata result authorizes none of those side effects.

## Operator surface

```sh
phoenix-probe-admin verify-image --repository HOST/NAME --version VERSION \
  --architecture amd64 --index PATH --index-sha256 HEX --index-size BYTES \
  --manifest PATH --config PATH
```

Use `--architecture arm64` for the other supported Linux target.

The explicit local command dispatches before hub environment/DB loading, uses
regular-file nonblocking input opening on Linux/macOS and a 30-second context,
and reports `image_metadata_verified` in an explicit DTO. It always reports
`layers_verified=false` and `runtime_version_verified=false`. No Docker daemon,
registry credentials, source filesystem writes, SSH, network or runtime is used.

## Ownership and scope

Main owns new domain/port/service, standard-library metadata adapter and tests,
local CLI/bootstrap/dispatch and documentation. Reuse the existing regular-file
source-opening policy while preserving binary verification behavior. No schema,
new dependencies, HTTP/UI surface, background work or M7 changes.

## Acceptance requirements

Cover OCI and Docker schema-2 chains on both architectures; BuildKit attestation
selection; wrong trusted digest/size and manifest/config substitutions; duplicate
platforms; unsupported platform/variant; descriptor URLs/foreign layers/artifacts;
component/version/entrypoint/user/Cmd/rootfs mismatch; duplicate/case-aliased JSON,
UTF-8/depth/token limits; cancellation before/mid-read and parsing; all-input
ownership; input immutability; exact service result identity; typed secret-safe
errors; and actual subprocess CLI dispatch before invalid hub settings. Exercise
critical assertions with negative overlays and an unmodified repeated control.

These tests use real digest-linked JSON fixture bytes, not a Docker pull or a
publisher signature. No real-image installation or live registry claim is implied.
Full applicable build/race/vet/format gates and explicit skip reporting remain
required. The subsequent [internal install journal](M8_INSTALL_JOURNAL.md) records
requested artifact/resource intent only, with no installer or success endpoint.
Actual remote ownership/delivery/execution and full M8/M6 release gates remain open.

## Executed evidence

The source is implemented locally on `feat/m8-ssh-preflight` after `2155ef1`.
No commit, push, deployment or live provisioning is implied. Private ignored
artifacts are in `tmp/m8-image-metadata-verification/`; overlays live under
`.overlays/` so `go ... ./...` cannot discover them as extra packages.

Frozen Go/SQL/Python/shell/module source manifest:
`2734574db263a93a516eb94a1f252e95f05d26ba737719deacbe463d7e0b63e1`.

Executed with cached Go 1.26.9 and `GOENV=off GOTOOLCHAIN=local GOPROXY=off
GOSUMDB=off`; no inherited database DSN or registry credentials were used.

Completed on that frozen source:

| Executed check | Result |
|---|---|
| `go build ./...` | Passed |
| `go vet ./...` | Passed |
| `gofmt -l internal/ cmd/` | Empty |
| `git diff --check` | Passed; final documentation recheck recorded separately |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...` | Passed |
| `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...` | Passed |
| M6 strict-parser regressions | 17 passed |
| Focused image + binary race control, `-count=3` | 564 named passes; zero failures/skips |
| Eleven isolated negative overlays | All caught by intended assertions; no compiler errors, panics, race reports or test timeouts counted as catches |
| `go test -race -count=1 -json -timeout 2400s -p 4 ./...` | Exit 0; 23 test packages; 5,770 named passes; zero failures; 1,845 seconds |
| Named skips in the default full run | 502: 497 explicitly missing MariaDB DSN; five existing non-MariaDB cases |
| Package-level skips | 14 packages with no test files, not skipped named tests |
| Source manifest after full run | Unchanged |

Mutation coverage: exact digest, exact size, unique target, config version label,
duplicate decoded JSON keys, interpreted-field case aliases, JSON depth/token
limits, cancellation-driven input closure, exact service result pin and dispatch
before hub loading. The negative fixtures preserve an otherwise valid chain;
checks are not masked by unrelated missing metadata. `preliminary-01/` retains
an earlier 157-pass focused run. `run-01/` was deliberately interrupted to remove
a dead test helper and specialize a single-call helper before final acceptance;
its partial full-run events and original control/mutation evidence remain
preserved, not counted as a completed gate. `run-02/` reruns all stages, control
and mutations on the final source before the full race suite.

The completed full run exercises the default/SQLite configuration and has **no
MariaDB DSN**. The five other named skips are `TestConfigGroupCycleHelperProcess`,
`TestDatabaseChecker_Check_MongoDB_RealServer`, `TestEdgeCheckCrashChild`,
`TestEdgeDiskFullCriticalCommit`, and `TestTelegramSender_Send_DownSeverity`.
Neither the default full run nor stateless metadata fixtures establish broad
MariaDB acceptance; the earlier M7 blocker remains unresolved.

The read-only review reported no blocker in its pre-cleanup code-inspection
scope; it did not execute tests. Fixture tests do not prove correspondence with an actually built
or published image, including inherited base-image config. That real-image smoke
check is required before installer use. No registry request, Docker daemon/pull,
layer bytes, cosign/publisher proof, SSH mutation, VM, install, enrollment or config
activation was exercised. The future installer must independently verify actual
pull/platform/config results and authenticated runtime evidence. The separate
[offline content seam](M8_IMAGE_CONTENT.md) now verifies supplied raw/gzip layer
hashes and bounded diff IDs, but retains no bytes and does not validate tar/rootfs
safety or close the actual built-image correspondence gate. The original
`verify-image` command remains metadata-only with its original non-claims.

`golangci-lint` and `govulncheck` are unavailable locally. Vet/formatting are the
project fallback, not full lint/vulnerability acceptance. Frontend/Helm and
M7/distributed storage are untouched. Existing M7 broad MariaDB failure and full
M8/M6 operational/release/deployment-approval gates remain open.
