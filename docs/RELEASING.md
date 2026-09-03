# Releasing Uptime Phoenix

> **CI is restored (owner, 2026-07-28).** `.github/workflows/ci.yml` gates PRs and
> `main`. Release automation lives in `.github/workflows/release.yml`: every run
> **dry-runs** first; **publish** happens on a `v*` tag push (full release) or a
> `workflow_dispatch` run with `publish=true` against an existing tag (selective
> release — see below). The local script `./scripts/release/dry-run.sh` remains the
> offline equivalent and **never publishes**.

This document describes how Uptime Phoenix prepares release artifacts and how optional
publish to GHCR / Helm OCI / GitHub Releases is owner-gated. `LICENSE` (MIT) is
present at the repo root.

## Hard rules

1. **No publish from dry-run.** The dry-run script and the `dry-run` job never push
   images, never push Helm charts, never create git tags, and never open a GitHub
   Release.
2. **Publish requires an existing tag + approval.** Publish happens on a `v*` tag
   push (full release), **or** a `workflow_dispatch` run with `publish=true` — but
   dispatch-publish only proceeds when the git tag `v<version>` **already exists**
   and is checked out (see rule 3). Publish always requires approval on the GitHub
   Environment named `release` (configure required reviewers in repo settings).
   Dispatch-publish lets you build only the artifacts you tick (default: chart +
   split images) so a patch release does not rebuild every image and binary.
3. **Publish is bound to the tag commit.** On any publish path (tag-push or
   dispatch `publish=true`), both the dry-run and publish jobs check out
   `refs/tags/v<version>` and assert `HEAD == tag^{commit}` before any build or
   push. Branch code cannot be published under a different tag, and a
   dispatch-publish can never publish an arbitrary branch head.
4. **This workflow never creates git tags.** The owner creates and pushes tags by
   hand (`git tag` + `git push origin v…`).
5. **Version strings are validated.** SemVer-compatible `X.Y.Z` or
   `X.Y.Z-prerelease` only; inputs reach shell via `env:`, never raw expression
   interpolation into `run:` scripts.
6. **LICENSE is resolved.** MIT at the repo root; remaining blockers are human
   approvals (signing policy, package naming, etc.).

## Snapshot version flow

Supply one version string for the entire dry-run (example `0.0.0-snapshot.1`,
`0.1.0-rc.0`, or `0.1.0`). That exact string is stamped into:

| Surface | Mechanism |
|---|---|
| Go binaries | `-ldflags "-X github.com/fiztoz/uptime-phoenix/internal/version.Version=<v>"` |
| Container image labels | `org.opencontainers.image.version=<v>` build-arg `VERSION` |
| Image tags (local only for dry-run) | `uptime-phoenix:<v>-linux-<arch>` |
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
| `workflow_dispatch` (`publish=false`, default) | **Dry-run only.** Inputs: `version` (required), `skip_docker`. Never publishes. |
| `workflow_dispatch` (`publish=true`) | **Selective publish** of the ticked artifacts (`build_chart`/`build_split` default on; `build_all_in_one`/`build_binaries` default off). Requires the `v<version>` tag to already exist + Environment approval. Bound to the tag commit. **Use workflow from** must be the tag itself (`vX.Y.Z`), not `main` — see below. |
| `push` of tags matching `v*` | Dry-run bound to that tag commit, then **publish all artifacts** (after Environment approval). |

### Dry-run job

1. Checkout. On tag-push events, re-check out `refs/tags/v…` and assert
   `HEAD == tag^{commit}`.
2. Validate the version string (SemVer-compatible) via `env` (no raw injection).
3. Setup Go / pinned Bun / Helm / Docker buildx + QEMU (unless `skip_docker`).
4. Install Syft from a **versioned release with SHA-256 verification** (optional;
   dry-run continues without SBOMs if install fails).
5. Build `web/dist` for `//go:embed` and `USE_PREBUILT_WEB=1`.
6. Run `VERSION=<version> ./scripts/release/dry-run.sh` (sets `SKIP_DOCKER=1` when
   the dispatch input asks for it).
7. Upload `dist/release-<version>/` as a workflow artifact (14-day retention).
8. Fail if dry-run fails.

Dry-run permissions are read-only (`contents: read`, `packages: read`). Nothing is
pushed. Privileged third-party Actions in this workflow are pinned to full commit
SHAs (see comments next to each `uses:` line).

### How to trigger a dry-run only

**GitHub UI:** Actions → **Release** → Run workflow → set `version`, leave
`publish` unticked. Optionally set `skip_docker` for a faster binary/helm-only run.

**CLI:**

```bash
gh workflow run release.yml -f version=0.0.0-snapshot.2
# Faster (no Docker multi-arch):
gh workflow run release.yml -f version=0.0.0-snapshot.2 -f skip_docker=true
```

Download the artifact from the run summary when it finishes.

### How to trigger a selective (fast) publish

Use this for patch releases where you do not want to rebuild every image and
binary. The tag must already exist (create + push it by hand first).

**GitHub UI:** Actions → **Release** → Run workflow → set `version`, tick
`publish`, and tick only the artifacts you want (defaults: `build_chart` +
`build_split` on; `build_all_in_one` + `build_binaries` off). Approve the
`release` Environment when prompted.

**CLI:**

```bash
# Owner has already pushed the tag: git tag v0.3.7 && git push origin v0.3.7
# Chart + split images only (the defaults):
gh workflow run release.yml -f version=0.3.7 -f publish=true

# Everything (equivalent to a tag-push full release):
gh workflow run release.yml -f version=0.3.7 -f publish=true \
  -f build_all_in_one=true -f build_binaries=true

# Chart only (e.g. a values/template-only fix):
gh workflow run release.yml -f version=0.3.7 -f publish=true -f build_split=false
```

### Publish job (owner-gated)

Runs only when:

1. The resolved `publish` flag is `1` — a `v*` **tag push**, or a
   `workflow_dispatch` run with `publish=true`, **and**
2. Dry-run succeeded, **and**
3. The GitHub Environment **`release`** is approved (configure **required
   reviewers** on that environment in Settings → Environments), **and**
4. Checkout is forced to `refs/tags/v<version>` with
   `HEAD == $(git rev-list -n1 v<version>)` (fails closed if the tag is missing
   or does not match — this is what makes a dispatch-publish safe).

Each publish step is gated by its artifact flag, so an unticked artifact skips its
build, login, and sign steps entirely:

1. Log in to GHCR with `GITHUB_TOKEN` (only when an image is selected).
2. Multi-arch (`linux/amd64,linux/arm64`) buildx **push** from the **tag tree**:
   - `ghcr.io/<owner>/uptime-phoenix:<version>` and `:latest` (all-in-one
     `Dockerfile`) — only when `build_all_in_one` is ticked.
   - `ghcr.io/<owner>/uptime-phoenix-{api,worker,web}:<version>` and `:latest`
     (`Dockerfile.split`) — only when `build_split` is ticked.
   - `<owner>` is `github.repository_owner` lowercased (chart defaults use
     `ghcr.io/fiztoz/...`; forks publish under their own namespace).
3. **Cosign keyless sign** each pushed image digest (Sigstore OIDC via
   `id-token: write`). Normal `docker pull` is unchanged.
4. `helm push` the chart package to `oci://ghcr.io/<owner>/charts` — only when
   `build_chart` is ticked.
5. Create/update a GitHub Release for tag `v<version>`, attaching the selected
   artifacts (chart `.tgz` when `build_chart`; binaries + `SHA256SUMS` when
   `build_binaries`) and always `INVENTORY.md`. Unselected artifact directories are
   pruned before the release step, so their globs simply match nothing.

### Optional: verify a published image (cosign)

Install [cosign](https://docs.sigstore.dev/cosign/system_config/installation/), then:

```bash
# Replace owner/version/digest as published
IMAGE=ghcr.io/fiztoz/uptime-phoenix
# Prefer digest from the release notes / GHCR UI, or:
DIGEST=$(crane digest "${IMAGE}:0.1.0")   # or docker buildx imagetools inspect

cosign verify \
  --certificate-identity-regexp 'https://github.com/fiztoz/uptime-phoenix/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "${IMAGE}@${DIGEST}"
```

If you skip verification, pull and run the image as usual — signing does not
change tags or require extra runtime config.

**How to publish:**

```bash
# Owner creates and pushes the tag by hand (workflow never creates tags)
git tag v0.1.0
git push origin v0.1.0
# Tag push triggers dry-run + publish (after Environment approval).
```

To re-publish the same version after a failed run, re-run the failed jobs from the
Actions UI on the original run **only if that run's ref is already the tag**
(`head_branch: vX.Y.Z`). Otherwise trigger a fresh `workflow_dispatch` with
`publish=true` **from the tag ref**, not from `main`:

```bash
# "Use workflow from" = the tag. The `release` environment only allows tags
# matching v*; GitHub checks github.ref (the dispatched ref), not the tag the
# job later checks out. Dispatch-from-main fails publish immediately (empty
# steps, no logs).
gh workflow run release.yml --ref v0.1.0 \
  -f version=0.1.0 \
  -f publish=true \
  -f build_chart=true \
  -f build_split=true \
  -f build_all_in_one=false \
  -f build_binaries=false
```

In the Actions UI: **Actions → Release → Run workflow → Use workflow from →
Tags → `v0.1.0`**. Move the tag only with extreme care — prefer a new patch
version.

No secrets beyond `GITHUB_TOKEN` are required for public GHCR packages in the same
repo. The workflow never force-pushes and never commits to `main`.

### Environment protection (required setup)

Create a GitHub Environment named **`release`** and enable:

- Required reviewers (owner or trusted maintainers)
- Optional: limit deployment branches/tags if your plan supports it. This repo
  allows tags matching `v*` only. That is why dispatch-publish must be started
  **from the tag** (`--ref vX.Y.Z` / Use workflow from = Tags). Adding `main`
  to the policy would let you dispatch from `main`, but GitHub would then run
  the workflow YAML currently on `main` rather than the tagged copy.

Environment approval gates **every** publish, whether triggered by a tag push or a
`workflow_dispatch` with `publish=true`. Protect the environment before the first
real release.

## Artifact inventory

Under `dist/release-<version>/` (local or CI artifact):

| Path | Contents |
|---|---|
| `binaries/` | CGO-free cross-compiled binaries for matrix below + `SHA256SUMS` |
| `binaries/uptime-phoenix_<v>_<os>_<arch>[.exe]` | All-in-one server (`cmd/app`) |
| `binaries/uptime-phoenix-api_<v>_<os>_<arch>[.exe]` | API binary |
| `binaries/uptime-phoenix-worker_<v>_<os>_<arch>[.exe]` | Worker binary |
| `binaries/uptime-phoenix-config_<v>_<os>_<arch>[.exe]` | Config-as-code CLI (`cmd/phoenix-config`) |
| `binaries/uptime-phoenix-kuma-import_<v>_<os>_<arch>[.exe]` | Kuma → Uptime Phoenix converter |
| `charts/` | `helm package` output (`.tgz`) with stamped `appVersion` |
| `helm-template.yaml` | `helm template` render for inspection |
| `sbom/*.spdx.json` | Syft SBOMs for binaries and images when syft is available |
| `images/uptime-phoenix-from-image-linux-<arch>` | Binary extracted from a local image for arch proof |
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
  an arm64 binary (proved by extracting `/uptime-phoenix` and running `file`).
- **Split** (`Dockerfile.split`): `api`, `worker`, and `web` targets, same
  multi-arch args. Dry-run loads images locally without pushing; publish pushes
  multi-arch manifests to GHCR.

## Multi-arch proof (not just manifest acceptance)

A green `docker buildx build --platform linux/arm64` is not enough if the
Dockerfile hardcodes `GOARCH=amd64` (historical bug). The dry-run:

1. Builds with `--build-arg TARGETARCH=arm64` / `GOARCH=${TARGETARCH}`.
2. `docker create` + `docker cp` extracts `/uptime-phoenix`.
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
| Image / binary names aligned with repo (`uptime-phoenix*`) | Owner | **Done — 2026-08** (GHCR + release artifacts) |
| Provenance / cosign signing policy | Owner | **Done — 2026-08** (keyless cosign on publish; verify optional for users) |
| Tag-bound publish (tag-push + dispatch bound to existing tag) | Owner | **Done — 2026-08** (see Hard rules) |
| Version via env + SemVer regex | Owner | **Done — 2026-08** |
| Pinned Actions (release.yml) + pinned Bun | Owner | **Done — 2026-08** (full digests for images still open) |
| No curl\|sh from `main` (syft / actionlint) | Owner | **Done — 2026-08** (versioned + SHA-256) |
| `.dockerignore` excludes secrets | Owner | **Done — 2026-08** |
| Dependabot version updates (`.github/dependabot.yml`) | Owner | **Done — 2026-08** (majors ignored; group minors) |
| Environment `release` requires owner review + `v*` tags only | Owner | **Done — 2026-08** |
| Frontend toolchain (Vite 8 / Kit 2.70 / plugin-svelte 7 / TS 6) | Owner | **Done — 2026-08** |
| Go toolchain `1.26.6` aligned (`go.mod` + `GOTOOLCHAIN` + Docker) | Owner | **Done — 2026-08** |
| Unused web deps removed (zod/superforms/adapter-node/…) | Owner | **Done — 2026-08** |

## Release procedure (recommended)

1. Run `make gate-full` (see `docs/TESTING.md`) on the commit being released, and the
   MariaDB repository contract against a throwaway DB if CI did not already cover that
   commit.
2. Dry-run:
   - **Local:** `VERSION=0.1.0 ./scripts/release/dry-run.sh` and inspect
     `dist/release-0.1.0/INVENTORY.md`, **or**
   - **CI:** `gh workflow run release.yml -f version=0.1.0` and
     download the artifact.
3. Clear remaining promotion blockers above.
4. Owner creates and pushes `git tag v0.1.0 && git push origin v0.1.0`.
5. Approve the `release` Environment deployment when GitHub prompts.
6. Verify GHCR tags, Helm OCI package, and the GitHub Release assets.

No agent should create or push a tag, publish an image, or publish a Helm chart
outside this owner-gated path.

## Cleanup local dry-run artifacts

```bash
# Everything under dist/release-* plus matching local uptime-phoenix* image tags
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
- [ ] Chart `image.repository` / consumers accept `ghcr.io/<owner>/uptime-phoenix`
- [ ] Rollback steps above are understood

Until publish is intentionally triggered: **dry-run only.**
