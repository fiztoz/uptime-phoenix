# Dependabot upgrade plan (open PRs)

> **Date:** 2026-08-08  
> **Repo:** [fiztoz/uptime-phoenix](https://github.com/fiztoz/uptime-phoenix)  
> **Scope:** All Dependabot PRs opened after Dependabot was enabled on public `main`.  
> **Status of `main`:** CI green (includes Prettier fix for `web/static/docs/*`).

This document is the working plan for every open dependency PR: what is affected,
recommended action, order of work, and how to handle **major** upgrades without
breaking the hobby baseline.

---

## 1. Preconditions (do once before any merge)

| Step | Action | Why |
|---|---|---|
| 1 | Confirm `main` CI is green | Required by branch protection |
| 2 | Close/ignore upgrades that are explicitly deferred | Avoid spending CI time rebasing work that will not merge |
| 3 | Rebase only the active merge candidates onto current `main` | Most red CI is stale Prettier failures from before `a8ecd48` |
| 4 | Wait for CI on each rebased PR | Branch protection requires all 7 checks |
| 5 | Run `make gate-full` locally before each merge | Required by `docs/TESTING.md`; CI is not a substitute for the local gate |

**Rebase command (per PR):** comment on the PR:

```text
@dependabot rebase
```

Or close + let Dependabot recreate after the next schedule.

---

## 2. Inventory of open PRs

| PR | Dependency | Current → Proposed | Ecosystem | Risk | Recommended action |
|---|---|---|---|---|---|
| [#5](https://github.com/fiztoz/uptime-phoenix/pull/5) | `prometheus-community/pro-bing` | `0.9.0` → `0.9.1` | Go | Low | **Merge after rebase** |
| [#9](https://github.com/fiztoz/uptime-phoenix/pull/9) | `golang.org/x/crypto` | `0.53.0` → `0.54.0` | Go | Low | **Merge after rebase** |
| [#12](https://github.com/fiztoz/uptime-phoenix/pull/12) | `go.mongodb.org/mongo-driver/v2` | `2.7.0` → `2.8.0` | Go | Low–med | **Hold after rebase** until a known-good MongoDB smoke passes |
| [#13](https://github.com/fiztoz/uptime-phoenix/pull/13) | `google.golang.org/grpc` | `1.82.1` → `1.83.0` | Go | Low–med | **Merge after rebase** + gRPC checker tests |
| [#8](https://github.com/fiztoz/uptime-phoenix/pull/8) | `prometheus/client_golang` | `1.20.5` → `1.24.1` | Go | Medium | **Merge after rebase** if metrics tests pass |
| [#14](https://github.com/fiztoz/uptime-phoenix/pull/14) | `docker/build-push-action` | `v6` → `v7` | Actions (CI) | Medium | Merge after rebase; CI-only usage |
| [#7](https://github.com/fiztoz/uptime-phoenix/pull/7) | `nginx` image | `1.27-alpine` → `1.31-alpine` | Docker | Medium | Merge after Docker CI plus an nginx runtime smoke |
| [#2](https://github.com/fiztoz/uptime-phoenix/pull/2) | `docker/setup-qemu-action` | `3.6.0` → `4.2.0` | Actions (release pin) | Medium | Keep Dependabot SHA pin; validate with Docker enabled |
| [#6](https://github.com/fiztoz/uptime-phoenix/pull/6) | `softprops/action-gh-release` | `2.3.2` → `3.0.2` | Actions (release pin) | Medium–high | Hold until a protected prerelease exercises the publish job |
| [#4](https://github.com/fiztoz/uptime-phoenix/pull/4) | `actions/upload-artifact` | `4.6.2` → `7.0.1` | Actions (release pin) | High | Keep Dependabot SHA pin; test upload/download round trip |
| [#10](https://github.com/fiztoz/uptime-phoenix/pull/10) | `actions/download-artifact` | `4.3.0` → `8.0.1` | Actions (release pin) | High | Pair validation with #4; dispatch alone does not execute it |
| [#11](https://github.com/fiztoz/uptime-phoenix/pull/11) | `golang` image | `1.25-alpine` → `1.26-alpine` | Docker | Medium–high | Treat as a release-compiler upgrade; full test suite under Go 1.26 first |
| [#3](https://github.com/fiztoz/uptime-phoenix/pull/3) | `node` image | `22-alpine` → `25-alpine` | Docker | High | **Defer / close** (major base jump) |
| [#15](https://github.com/fiztoz/uptime-phoenix/pull/15) | `@sveltejs/vite-plugin-svelte` | `5.x` → `7.x` | Frontend major | **High** | **Do not merge alone** — Wave F |
| [#16](https://github.com/fiztoz/uptime-phoenix/pull/16) | `prettier-plugin-svelte` | `3.x` → `4.x` | Frontend major | High | Wave F |
| [#17](https://github.com/fiztoz/uptime-phoenix/pull/17) | `zod` | `3.x` → `4.x` | Frontend major | **High (breaking)** | Wave F + code migration |
| [#18](https://github.com/fiztoz/uptime-phoenix/pull/18) | `vite` | `6.x` → `8.x` | Frontend major | **High** | Wave F |
| [#19](https://github.com/fiztoz/uptime-phoenix/pull/19) | `typescript` | `5.x` → `7.x` | Frontend major | **High** | Wave F |

---

## 3. Affected surfaces by dependency class

### 3.1 Go modules

| Package | Used for | Test gate after bump |
|---|---|---|
| `pro-bing` | ICMP / ping checker | `go test ./internal/adapters/checker/ -run Ping` |
| `x/crypto` | bcrypt, crypto helpers | `go test ./internal/adapters/auth/...` |
| `mongo-driver/v2` | Database monitor (MongoDB) | Checker tests plus a known-good MongoDB connection smoke; the current bad-host test is not sufficient |
| `grpc` | gRPC health checker | `go test ./internal/adapters/checker/ -run GRPC` |
| `client_golang` | Prometheus `/metrics` | `go test ./internal/adapters/metrics/...` + manual `/metrics` smoke |

**Files typically touched:** `go.mod`, `go.sum` only (unless APIs change).

### 3.2 Docker base images

| Image | Files | Notes |
|---|---|---|
| `golang:1.25-alpine` | `Dockerfile`, `Dockerfile.split` | A 1.26 image changes the release compiler; `go.mod` may remain at the supported minimum |
| `node:22-alpine` | `Dockerfile`, `Dockerfile.split` | Runs the source-build Vite path through `npm run build`; `USE_PREBUILT_WEB=1` bypasses it |
| `nginx:1.27-alpine` | `Dockerfile.split` (web target) | Split web tier only |
| `gcr.io/distroless/static-debian12` | both Dockerfiles | Not in current PR list |

### 3.3 GitHub Actions

| Action | Used in | Pin style today |
|---|---|---|
| `docker/setup-qemu-action` | `release.yml` (dry-run + publish) | **Full commit SHA** (`v3.6.0`) |
| `docker/build-push-action` | `ci.yml` docker job | Floating `@v6` |
| `actions/upload-artifact` | `release.yml` dry-run | **Full commit SHA** (`v4.6.2`) |
| `actions/download-artifact` | `release.yml` publish | **Full commit SHA** (`v4.3.0`) |
| `softprops/action-gh-release` | `release.yml` publish | **Full commit SHA** (`v2.3.2`) |

**Important:** Dependabot currently updates the full SHA and version comment correctly for the
SHA-pinned actions in `release.yml`. Preserve that pin style and verify both parts changed:

```yaml
uses: org/action@<full-sha> # vX.Y.Z
```

If a proposed SHA ever needs independent verification, resolve it with:

```bash
gh api repos/<org>/<action>/commits/vX.Y.Z --jq .sha
```

### 3.4 Frontend (web/) — major stack

| Package | Role in Phoenix | Coupled with |
|---|---|---|
| `typescript` | `svelte-check`, `tsconfig` | `typescript-eslint`, Svelte 5 types |
| `vite` | Dev server + production build | `@sveltejs/kit`, `@sveltejs/vite-plugin-svelte` |
| `@sveltejs/vite-plugin-svelte` | Svelte compilation in Vite | `svelte`, `vite` |
| `prettier-plugin-svelte` | `bun run lint` / format | `prettier` |
| `zod` | Forms (`sveltekit-superforms`), validation schemas | Superforms, any `z.` schemas under `web/src` |

Only Vite and `@sveltejs/vite-plugin-svelte` are tightly coupled. Split the other majors into
small compatibility groups so failures remain attributable (see Wave F).

---

## 4. Execution waves

### Wave 0 — Hygiene (same day)

1. Close/ignore #3 and #15–#19 before spending CI capacity on rebases.
2. Rebase only #5, #9, #12, #13, #8, #7, and #14 onto green `main`.
3. Confirm frontend failures from Prettier docs are gone on the rebased candidates.
4. Leave #2, #4, #6, #10, and #11 open but held until their additional gates below are available.

### Wave A — Safe Go patches (1–2 hours)

**Order (smallest blast radius first):**

1. #5 pro-bing  
2. #9 x/crypto  
3. #13 grpc  
4. #12 mongo-driver  
5. #8 client_golang  

**Per-PR checklist:**

- [ ] Rebased onto `main`
- [ ] CI: all 7 jobs green  
- [ ] `make gate-full` passes locally (required, not optional)
- [ ] #12 additionally passes a known-good MongoDB connection smoke
- [ ] Merge (squash OK under branch protection)

### Wave B — Docker bases that stay on current majors (half day)

1. #7 nginx `1.27 → 1.31`  
   - Gate: CI `docker` job + start the split web container and run `nginx -t` / an HTTP smoke; a build alone does not parse the runtime config
2. #11 golang image `1.25 → 1.26`  
   - This changes the compiler and standard library used for release binaries even if `go.mod` remains `go 1.25.12`
   - Run the complete gate under Go 1.26 before merging; do not raise the `go` directive solely to match the builder image
   - If the project is not ready to support Go 1.26-built releases, close #11 and keep `golang:1.25-alpine` intentionally
3. #3 node `22 → 25` — **defer** (see Wave D)

### Wave C — GitHub Actions (half day)

Keep the existing Dependabot PRs: they already preserve the full SHA pins in `release.yml`.
Do not combine unrelated action upgrades into one large change; separate PRs make release failures
attributable. Merge #14 independently after rebase because its CI Docker job directly exercises it.

| Action | Target | Also update |
|---|---|---|
| `setup-qemu-action` | #2 / stable 4.x | Run dispatch with Docker enabled; both SHA-pinned occurrences must match |
| `upload-artifact` / `download-artifact` | #4 + #10 compatibility validation | Add or temporarily run an unprivileged upload → download round trip |
| `action-gh-release` | #6 / 3.x | Validate through a protected prerelease; workflow dispatch never executes this action |
| `build-push-action` | #14 / v7 | Merge separately after the normal Docker CI job passes |

**Validation:**

```bash
# Local
actionlint .github/workflows/*.yml   # or CI actionlint job

# CI must be green

# Release dry-run with Docker enabled (no publish)
gh workflow run release.yml -f version=0.0.0-snapshot.deps -f skip_docker=false
```

The dispatch above exercises the dry-run QEMU and upload steps. It does **not** enter the
tag-only `publish` job, so it does not validate `download-artifact` or
`softprops/action-gh-release`. Do not treat a green dispatch as evidence for those two actions.

### Wave D — Deferred base images

| PR | Decision |
|---|---|
| #3 node 22→25 | **Close / ignore major** until explicit need. Node 22 is LTS-class, and the Docker source-build stage actually runs Vite under Node via `npm run build`. |

### Wave F — Frontend majors (dedicated effort, 1–3 days)

**Close or ignore majors on PRs #15–#19.** Reintroduce them later as small compatibility groups,
not as five independent Dependabot merges or one all-encompassing frontend branch.

#### F.1 Compatibility groups (proposed)

1. `prettier-plugin-svelte` 4.x — formatting/lint tooling only; review the formatting diff.
2. Zod 4 + a `sveltekit-superforms` version that explicitly supports it — schema migration.
3. Vite + `@sveltejs/vite-plugin-svelte` + any required Kit/Svelte peer updates — build-tool chain.
4. TypeScript 7 + matching `typescript-eslint`, Svelte checker, and Node types — compiler/tooling chain.

Each group gets its own branch, gate, and merge so a regression has one compatibility edge.

#### F.2 Target stack (proposed)

Coordinate versions that are known to work together (adjust after reading each release note):

| Package | From (approx locked) | Toward | Notes |
|---|---|---|---|
| `typescript` | 5.x | **5.9.x latest only first**, or 6.x if Svelte ecosystem ready | TS **7** is a large jump; prefer stepwise 5→6 before 7 |
| `vite` | 6.x | 7.x then 8.x **or** single hop if Kit supports it | Check `@sveltejs/kit` peer range |
| `@sveltejs/vite-plugin-svelte` | 5.x | Match Vite major | Keep with Vite |
| `prettier-plugin-svelte` | 3.x | 4.x | Run `bun run format` after |
| `zod` | 3.x | 4.x | **Code changes required** |

Also re-check peers (not in current PR list but often forced):

- `@sveltejs/kit`
- `svelte` 5.x
- `sveltekit-superforms` (Zod 4 support)
- `typescript-eslint`
- `eslint-plugin-svelte`

#### F.3 Zod 4 impact (code)

Search and migrate:

```bash
rg -n "from 'zod'|from \"zod\"|z\." web/src --glob '!**/paraglide/**'
```

Likely touch areas:

- Form schemas used with `sveltekit-superforms`
- API payload validation helpers
- Any `z.object` / `z.string()` refinements that changed error-map or default APIs in Zod 4

Follow [Zod 4 migration guide](https://zod.dev) (breaking changes around error formatting, `z.record`, defaults, etc.).

#### F.4 Frontend validation gate

```bash
cd web
bun install --frozen-lockfile   # after lockfile update
bun run check                  # paraglide + svelte-check (type gate)
bun run test
bun run build
bun run lint
bun run test:e2e               # needs backend; or CI e2e job
```

Repo gate:

```bash
make gate-fast                 # or make gate-full
```

Docker gates (both paths are required for build-tool or Node changes):

```bash
# Build the frontend inside Docker under the selected Node image.
docker build --build-arg USE_PREBUILT_WEB=0 -t uptime-phoenix:web-source .

# Preserve the release path that copies an already-built web/dist.
docker build --build-arg USE_PREBUILT_WEB=1 -t uptime-phoenix:web-prebuilt .
```

#### F.5 Wave F exit criteria

- [ ] `bun run check` zero errors  
- [ ] Unit + e2e green  
- [ ] Docker source build works (`USE_PREBUILT_WEB=0`)
- [ ] Docker prebuilt-web release path still works (`USE_PREBUILT_WEB=1`)
- [ ] No Dependabot major PRs left open for these packages (ignored or merged via the wave)

---

## 5. Per-wave PR / Dependabot hygiene

### After merging Wave A

```text
@dependabot rebase
```

on remaining PRs so they drop already-merged commits.

### Ignoring majors (recommended for hobby tempo)

On each of #3, #15–#19 (or after closing):

```text
@dependabot ignore this major version
```

### Optional Dependabot config follow-up

Update `.github/dependabot.yml` to reduce future floods:

```yaml
# Example: under npm (/web)
groups:
  frontend-minor:
    patterns: ["*"]
    update-types: ["minor", "patch"]
ignore:
  - dependency-name: "typescript"
    update-types: ["version-update:semver-major"]
  - dependency-name: "vite"
    update-types: ["version-update:semver-major"]
  - dependency-name: "zod"
    update-types: ["version-update:semver-major"]
  - dependency-name: "@sveltejs/vite-plugin-svelte"
    update-types: ["version-update:semver-major"]
  - dependency-name: "prettier-plugin-svelte"
    update-types: ["version-update:semver-major"]

# Example: under github-actions
groups:
  actions:
    patterns: ["*"]
```

Also consider `open-pull-requests-limit: 3` to keep the queue small.

---

## 6. Suggested calendar (hobby pace)

| When | Wave | Outcome |
|---|---|---|
| Day 0 | Wave 0 | Deferred majors ignored; active candidates rebased |
| Day 0–1 | Wave A | 4–5 Go patches on `main` |
| Day 1–2 | Wave B (nginx only) | Optional image bump |
| Day 2–3 | Wave C | Separately validated Actions PRs; release-only paths remain held until exercised |
| Later | Wave F | Small frontend compatibility groups when you want the investment |
| Later | golang 1.26 / node 25 | Only with intentional toolchain upgrade |

---

## 7. Decision summary

| Bucket | PRs | Do |
|---|---|---|
| **Merge soon** | #5, #9, #13, maybe #8 | Rebase → CI green → `make gate-full` → merge |
| **Needs a success-path smoke** | #12 | Add/run known-good MongoDB check before merge |
| **Merge carefully** | #7, #14 | Rebase → docker/CI green |
| **Validate exact release path** | #2, #4, #6, #10 | Keep SHA-pinned Dependabot PRs; merge only after the corresponding action actually runs |
| **Test new release compiler** | #11 | Full gate under Go 1.26; `go.mod` may remain at the supported minimum |
| **Defer / ignore major** | #3, #15, #16, #17, #18, #19 | Wave F or ignore |

---

## 8. Definition of done for this plan

- [x] Wave A merged (Go patches #5–#13 family)  
- [x] No stale red CI caused only by pre-Prettier `main`  
- [x] Release workflow still SHA-pinned and actionlint-clean (Actions majors merged with SHAs)  
- [x] Wave F (intentional): Vite 8 + `@sveltejs/vite-plugin-svelte` 7 + Kit 2.70 + prettier-plugin-svelte 4; TypeScript stays on 5.9 (eslint peer)  
- [x] Unused web deps removed: `zod`, `sveltekit-superforms`, `@sveltejs/adapter-node`, `d3-shape`, `clsx`, `tailwind-merge`  
- [x] Go toolchain aligned to **1.26.5** (`go.mod`, `GOTOOLCHAIN`, Docker already 1.26)  
- [x] Dependabot config tightened (ignore majors / group minors)  
- [ ] Optional: release dry-run after Actions majors (operational smoke)

---

## 9. Quick links

| Resource | URL |
|---|---|
| Open PRs | https://github.com/fiztoz/uptime-phoenix/pulls  
| Dependabot config | `.github/dependabot.yml` |
| CI workflow | `.github/workflows/ci.yml` |
| Release workflow | `.github/workflows/release.yml` |
| Releasing docs | `docs/RELEASING.md` |
| Frontend package | `web/package.json` |
| Go modules | `go.mod` |
