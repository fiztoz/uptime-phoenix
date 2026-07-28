# Releasing Phoenix (local dry-run first)

> **There is no CI.** `.github/` was deliberately removed (`9de75e9`, 2026-07-25) and is not
> coming back — this document previously described a GitHub Actions workflow
> (`release-dry-run.yml`) that no longer exists. Release is now a **local, manual,
> owner-triggered** procedure end to end: a human runs the script below on their own
> machine, inspects the output, and decides whether to promote it. Nothing runs
> automatically on push, on a schedule, or on tag creation.

This document describes how Phoenix **prepares** release artifacts without
publishing them. Publishing (GHCR push, Helm OCI push, GitHub Release, git tag) is
intentionally out of scope until the owner gives an explicit go-ahead — see
"Promotion blockers" below. `LICENSE` (MIT) landed 2026-07-25, closing the license
blocker that previously existed here.

## Hard rules

1. **No publish from dry-run.** The dry-run script never pushes images, never pushes
   Helm charts, never creates git tags, and never opens a GitHub Release.
2. **No automatic trigger of any kind.** The script is run by hand, by the owner (or an
   agent the owner explicitly asks to run it), on demand. There is no workflow file, no
   cron, no push hook.
3. **LICENSE is resolved.** `LICENSE` (MIT) is present at the repo root and referenced
   from `README.md` and `charts/phoenix/Chart.yaml`. The remaining promotion blockers
   (below) are explicit human approvals, not missing artifacts.

## Snapshot version flow

Supply one snapshot version string for the entire dry-run (example
`0.0.0-snapshot.1` or `0.1.0-rc.0`). That exact string is stamped into:

| Surface | Mechanism |
|---|---|
| Go binaries | `-ldflags "-X github.com/fiztoz/uptime-phoenix/internal/version.Version=<v>"` |
| Container image labels | `org.opencontainers.image.version=<v>` build-arg `VERSION` |
| Image tags (local only) | `phoenix:<v>-linux-<arch>` |
| Helm chart | `appVersion: "<v>"` on a **copy** of the chart before `helm package` |

No git tag is created for a dry-run. Semver tags remain a future, explicitly approved
promotion step — the owner creates and pushes them by hand; no automation does it.

## Local dry-run

```bash
# Full local dry-run (binaries + helm + docker when available)
VERSION=0.0.0-snapshot.1 ./scripts/release/dry-run.sh

# Binaries + helm only
VERSION=0.0.0-snapshot.1 SKIP_DOCKER=1 ./scripts/release/dry-run.sh
```

Requirements:

- Go toolchain matching `go.mod`
- `helm` for chart lint/package/template
- `docker` + `buildx` (+ QEMU for arm64) when not skipping images
- optional `syft` for SBOMs (`SKIP_SBOM=1` to skip)
- optional `file(1)` for architecture proof

Run this **before every release decision**, on the machine (or a machine) the owner
trusts to publish from. There is no second, automated copy of this check running
anywhere — if the dry-run isn't run, nothing has verified the release artifacts.

## Artifact inventory

Under `dist/release-<version>/`:

| Path | Contents |
|---|---|
| `binaries/` | CGO-free cross-compiled binaries for matrix below + `SHA256SUMS` |
| `binaries/phoenix_<v>_<os>_<arch>[.exe]` | All-in-one server (`cmd/app`) |
| `binaries/phoenix-api_<v>_<os>_<arch>[.exe]` | API binary |
| `binaries/phoenix-worker_<v>_<os>_<arch>[.exe]` | Worker binary |
| `binaries/kuma-import_<v>_<os>_<arch>[.exe]` | Kuma → Phoenix converter |
| `charts/` | `helm package` output (`.tgz`) with stamped `appVersion` |
| `helm-template.yaml` | `helm template` render for inspection |
| `sbom/*.spdx.json` | Syft SBOMs for binaries and images when syft is available |
| `images/phoenix-from-image-linux-<arch>` | Binary extracted from a local image for arch proof |
| `image-arch-proof.txt` | `file(1)` output proving arm64/amd64 match |
| `binary-arch.txt` | `file(1)` listing for cross-compiled binaries |
| `oci/*.oci.tar` | Multi-arch OCI layouts from buildx (local only, no push) |
| `INVENTORY.md` | Human-readable summary of the run |

### Binary matrix

Default platforms (CGO-free):

- `linux/amd64`, `linux/arm64`
- `darwin/amd64`, `darwin/arm64`
- `windows/amd64`, `windows/arm64`

If a combination fails to cross-compile, the dry-run records it under
**Failed cross-compiles** in `INVENTORY.md` and continues with the rest. Only
platforms that actually produce a binary are checksummed.

### Images

- **All-in-one** (`Dockerfile`): linux/amd64 and linux/arm64 with
  `TARGETOS`/`TARGETARCH` and `VERSION` ldflags. An arm64 buildx target must contain
  an arm64 binary (proved by extracting `/phoenix` and running `file`).
- **Split** (`Dockerfile.split`): `api`, `worker`, and `web` targets, same
  multi-arch args. The dry-run exports multi-arch OCI tarballs locally without
  pushing.

## Multi-arch proof (not just manifest acceptance)

A green `docker buildx build --platform linux/arm64` is not enough if the
Dockerfile hardcodes `GOARCH=amd64` (historical bug). The dry-run:

1. Builds with `--build-arg TARGETARCH=arm64` / `GOARCH=${TARGETARCH}`.
2. `docker create` + `docker cp` extracts `/phoenix`.
3. `file` must report `ARM aarch64` / `arm64` for the arm64 image and
   `x86-64` for the amd64 image.
4. Fails the run if the architecture does not match.

## Promotion blockers

Do **not** promote dry-run artifacts to a public release until every item is
cleared. All of these are now human, manual decisions — there is no workflow
gating them:

| Blocker | Owner | Status |
|---|---|---|
| Choose and add a `LICENSE` file | User | **Done — MIT, 2026-07-25** |
| Explicit approval to publish packages | User | Open |
| Create a real semver git tag | User | Open — created and pushed by hand, never by an agent |
| GHCR push permissions + package names | User | Open |
| Helm OCI repository + push | User | Open |
| GitHub Releases + checksum attach | User | Open |
| Image repository rename from placeholders (`ghcr.io/fiztoz/...` in values) | Chart consumers / User | Open |
| Provenance / cosign signing policy | User | Open |

## Release procedure (local, manual, owner-triggered)

1. Owner (or an agent explicitly instructed by the owner) runs `make gate-full`
   (see `docs/TESTING.md`) on the exact commit being considered for release, and
   the MariaDB repository contract + fresh-DB smoke suites against a throwaway
   database. Record the results — do not claim a pass that wasn't observed.
2. Owner runs the local dry-run script (above) with the intended version string
   and inspects `dist/release-<version>/INVENTORY.md`.
3. Owner decides, by hand, whether to clear the promotion blockers above.
4. If cleared: owner creates and pushes the git tag themselves, and runs whatever
   publish steps they choose (no publish automation exists yet — see the
   "future publish workflow" language below, which remains unimplemented and,
   given CI is gone for good, would also have to be a local script, not a
   GitHub Actions workflow).
5. No agent creates or pushes a tag, publishes an image, or publishes a Helm
   chart under any circumstance, regardless of how the dry-run looks.

## Cleanup local dry-run artifacts

```bash
# Everything under dist/release-* plus matching local phoenix* image tags
./scripts/release/clean.sh

# One snapshot only
VERSION=0.0.0-snapshot.1 ./scripts/release/clean.sh

# Entire dist/ tree (filesystem only)
./scripts/release/clean.sh --all-dist --no-images

# Preview without deleting
./scripts/release/clean.sh --dry-run
```

Never touches GHCR, Helm OCI, git tags, or GitHub Releases.

## Rollback

Dry-run creates no cluster or registry state. Rollback is:

1. Run `./scripts/release/clean.sh` (or delete local `dist/release-<version>/`).
2. No git tag to delete; no registry cleanup required.

If a **future** publish step is ever used and must be rolled back:

1. Untag / delete the erroneous GHCR tags.
2. Delete the GitHub Release and git tag **only** with human approval.
3. Restore the previous Helm chart version in environments that upgraded.
4. Prefer database restore from the pre-upgrade snapshot (see `docs/RUNBOOK.md`)
   over partial configuration re-import.

## Explicit no-publish checklist

Before any future publish step is added, verify it:

- [ ] Runs locally, by hand, at the owner's initiation — not on a schedule or a push
      (there is no CI to hook it into, by design)
- [ ] Is gated on an explicit human approval step, not an automated one
- [ ] Requests only the minimum credentials for that publish step, entered by the
      person running it
- [ ] Creates tags only after artifacts are verified
- [ ] Documents how to roll back tags and registry packages

Until then: **dry-run only.**
