# Releasing Phoenix

> **CI is restored (owner, 2026-07-28).** `.github/workflows/ci.yml` gates PRs and
> `main`. Release automation lives in `.github/workflows/release.yml`: every run
> **dry-runs** first; **publish** only happens when explicitly gated (see below).
> The local script `./scripts/release/dry-run.sh` remains the offline equivalent
> and **never publishes**.

This document describes how Phoenix prepares release artifacts and how optional
publish to GHCR / Helm OCI / GitHub Releases is owner-gated. `LICENSE` (MIT) is
present at the repo root.

## Hard rules

1. **No publish from dry-run.** The dry-run script and the `dry-run` job never push
   images, never push Helm charts, never create git tags, and never open a GitHub
   Release.
2. **Publish is owner-gated.** Publish requires either a `v*` tag push or
   `workflow_dispatch` with `publish=true`, plus approval on the GitHub Environment
   named `release` (configure required reviewers in repo settings).
3. **This workflow never creates git tags on `workflow_dispatch`.** The owner creates
   and pushes tags by hand. Publish on dispatch requires that `v<version>` already
   exists.
4. **LICENSE is resolved.** MIT at the repo root; remaining blockers are human
   approvals (signing policy, package naming, etc.).

## Snapshot version flow

Supply one version string for the entire dry-run (example `0.0.0-snapshot.1`,
`0.1.0-rc.0`, or `0.1.0`). That exact string is stamped into:

| Surface | Mechanism |
|---|---|
| Go binaries | `-ldflags "-X github.com/fiztoz/uptime-phoenix/internal/version.Version=<v>"` |
| Container image labels | `org.opencontainers.image.version=<v>` build-arg `VERSION` |
| Image tags (local only for dry-run) | `phoenix:<v>-linux-<arch>` |
| Helm chart | `appVersion: "<v>"` on a **copy** of the chart before `helm package` |

No git tag is created for a dry-run. Semver tags are an explicit owner step.

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

Run this before every release decision on a machine you trust. CI dry-run is a
second copy of the same script; local runs remain fully supported offline.

## GitHub Actions — release workflow

Workflow file: [`.github/workflows/release.yml`](../.github/workflows/release.yml).

### Triggers

| Trigger | Behaviour |
|---|---|
| `workflow_dispatch` | Always runs **dry-run**. Inputs: `version` (required), `publish` (default `false`), `skip_docker` (default `false`). |
| `push` of tags matching `v*` | Dry-run for version = tag with leading `v` stripped, then **publish**. |

### Dry-run job

1. Checkout, setup Go / Bun / Helm / Docker buildx + QEMU (unless `skip_docker`).
2. Build `web/dist` for `//go:embed` and `USE_PREBUILT_WEB=1`.
3. Run `VERSION=<version> ./scripts/release/dry-run.sh` (sets `SKIP_DOCKER=1` when
   the dispatch input asks for it).
4. Upload `dist/release-<version>/` as a workflow artifact (14-day retention).
5. Fail if dry-run fails.

Dry-run permissions are read-only (`contents: read`, `packages: read`). Nothing is
pushed.

### How to trigger a dry-run only

**GitHub UI:** Actions → **Release** → Run workflow → set `version`, leave
`publish` unchecked. Optionally set `skip_docker` for a faster binary/helm-only run.

**CLI:**

```bash
gh workflow run release.yml -f version=0.0.0-snapshot.2 -f publish=false
# Faster (no Docker multi-arch):
gh workflow run release.yml -f version=0.0.0-snapshot.2 -f publish=false -f skip_docker=true
```

Download the artifact from the run summary when it finishes.

### Publish job (optional, owner-gated)

Runs only when:

1. Dry-run succeeded, **and**
2. Either the event was a `v*` tag push, **or** dispatch had `publish=true`, **and**
3. The GitHub Environment **`release`** is approved (configure **required
   reviewers** on that environment in Settings → Environments), **and**
4. Git tag `v<version>` already exists (enforced in the job; never created by
   dispatch).

Publish steps:

1. Log in to GHCR with `GITHUB_TOKEN`.
2. Multi-arch (`linux/amd64,linux/arm64`) buildx **push**:
   - `ghcr.io/<owner>/phoenix:<version>` and `:latest` (all-in-one `Dockerfile`)
   - `ghcr.io/<owner>/phoenix-{api,worker,web}:<version>` and `:latest` (`Dockerfile.split`)
   - `<owner>` is `github.repository_owner` lowercased (chart defaults use
     `ghcr.io/fiztoz/...`; forks publish under their own namespace).
3. `helm push` the dry-run chart package to `oci://ghcr.io/<owner>/charts`.
4. Create/update a GitHub Release for tag `v<version>`, attaching binaries,
   `SHA256SUMS`, chart `.tgz`, and `INVENTORY.md` from the dry-run artifact.

**How to publish:**

```bash
# 1. Owner creates and pushes the tag by hand (workflow never does this on dispatch)
git tag v0.1.0
git push origin v0.1.0
# Tag push alone triggers dry-run + publish (after Environment approval).

# Or: tag already exists, re-run dispatch with publish:
gh workflow run release.yml -f version=0.1.0 -f publish=true
```

No secrets beyond `GITHUB_TOKEN` are required for public GHCR packages in the same
repo. The workflow never force-pushes and never commits to `main`.

### Environment protection (required setup)

Create a GitHub Environment named **`release`** and enable:

- Required reviewers (owner or trusted maintainers)
- Optional: deployment branches limited to tags / default branch

Without that, anyone with `workflow_dispatch` rights could publish when `publish=true`
if a tag already exists. Protect the environment before the first real release.

## Artifact inventory

Under `dist/release-<version>/` (local or CI artifact):

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
  multi-arch args. Dry-run loads images locally without pushing; publish pushes
  multi-arch manifests to GHCR.

## Multi-arch proof (not just manifest acceptance)

A green `docker buildx build --platform linux/arm64` is not enough if the
Dockerfile hardcodes `GOARCH=amd64` (historical bug). The dry-run:

1. Builds with `--build-arg TARGETARCH=arm64` / `GOARCH=${TARGETARCH}`.
2. `docker create` + `docker cp` extracts `/phoenix`.
3. `file` must report `ARM aarch64` / `arm64` for the arm64 image and
   `x86-64` for the amd64 image.
4. Fails the run if the architecture does not match.

## Promotion blockers

Do **not** promote dry-run artifacts to a public release until every item is cleared:

| Blocker | Owner | Status |
|---|---|---|
| Choose and add a `LICENSE` file | User | **Done — MIT, 2026-07-25** |
| Explicit approval to publish packages | User | Open — use Environment `release` reviewers |
| Create a real semver git tag | User | Open — created and pushed by hand |
| GHCR push permissions + package names | User | Workflow uses `GITHUB_TOKEN` + `packages: write` |
| Helm OCI repository + push | User | Workflow pushes `oci://ghcr.io/<owner>/charts` |
| GitHub Releases + checksum attach | User | Workflow attaches dry-run binaries/chart |
| Image repository rename from placeholders (`ghcr.io/fiztoz/...` in values) | Chart consumers / User | Open — publish uses current repo owner |
| Provenance / cosign signing policy | User | Open |

## Release procedure (recommended)

1. Run `make gate-full` (see `docs/TESTING.md`) on the commit being released, and the
   MariaDB repository contract against a throwaway DB if CI did not already cover that
   commit.
2. Dry-run:
   - **Local:** `VERSION=0.1.0 ./scripts/release/dry-run.sh` and inspect
     `dist/release-0.1.0/INVENTORY.md`, **or**
   - **CI:** `gh workflow run release.yml -f version=0.1.0 -f publish=false` and
     download the artifact.
3. Clear promotion blockers above.
4. Owner creates and pushes `git tag v0.1.0 && git push origin v0.1.0` (or re-runs
   dispatch with `publish=true` against an existing tag).
5. Approve the `release` Environment deployment when GitHub prompts.
6. Verify GHCR tags, Helm OCI package, and the GitHub Release assets.

No agent should create or push a tag, publish an image, or publish a Helm chart
outside this owner-gated path.

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

If a **published** release must be rolled back:

1. Untag / delete the erroneous GHCR tags (and Helm OCI package if needed).
2. Delete the GitHub Release and git tag **only** with human approval.
3. Restore the previous Helm chart version in environments that upgraded.
4. Prefer database restore from the pre-upgrade snapshot (see `docs/RUNBOOK.md`)
   over partial configuration re-import.

## Explicit publish checklist

Before each publish:

- [ ] Dry-run inventory inspected (local or CI artifact)
- [ ] Tag `v<version>` created and pushed by the owner (not by an agent ad hoc)
- [ ] Environment `release` has required reviewers enabled
- [ ] Chart `image.repository` / consumers accept `ghcr.io/<owner>/phoenix`
- [ ] Rollback steps above are understood

Until publish is intentionally triggered: **dry-run only.**
