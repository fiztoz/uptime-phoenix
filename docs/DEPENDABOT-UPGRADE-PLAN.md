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
| 2 | Rebase **all** open Dependabot PRs onto current `main` | Most red CI is stale Prettier failures from before `a8ecd48` |
| 3 | Wait for CI on each rebased PR | Branch protection requires all 7 checks |

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
| [#12](https://github.com/fiztoz/uptime-phoenix/pull/12) | `go.mongodb.org/mongo-driver/v2` | `2.7.0` → `2.8.0` | Go | Low–med | **Merge after rebase** + checker smoke |
| [#13](https://github.com/fiztoz/uptime-phoenix/pull/13) | `google.golang.org/grpc` | `1.82.1` → `1.83.0` | Go | Low–med | **Merge after rebase** + gRPC checker tests |
| [#8](https://github.com/fiztoz/uptime-phoenix/pull/8) | `prometheus/client_golang` | `1.20.5` → `1.24.1` | Go | Medium | **Merge after rebase** if metrics tests pass |
| [#14](https://github.com/fiztoz/uptime-phoenix/pull/14) | `docker/build-push-action` | `v6` → `v7` | Actions (CI) | Medium | Merge after rebase; CI-only usage |
| [#7](https://github.com/fiztoz/uptime-phoenix/pull/7) | `nginx` image | `1.27-alpine` → `1.31-alpine` | Docker | Medium | Merge after docker job green |
| [#2](https://github.com/fiztoz/uptime-phoenix/pull/2) | `docker/setup-qemu-action` | `3.6.0` → `4.2.0` | Actions (release pin) | Medium | **Manual pin update** in `release.yml`, not blind merge |
| [#6](https://github.com/fiztoz/uptime-phoenix/pull/6) | `softprops/action-gh-release` | `2.3.2` → `3.0.2` | Actions (release pin) | Medium–high | **Manual pin update** + release dry-run |
| [#4](https://github.com/fiztoz/uptime-phoenix/pull/4) | `actions/upload-artifact` | `4.6.2` → `7.0.1` | Actions (release pin) | High | Batch with #10; **update SHA pins** |
| [#10](https://github.com/fiztoz/uptime-phoenix/pull/10) | `actions/download-artifact` | `4.3.0` → `8.0.1` | Actions (release pin) | High | Batch with #4; **update SHA pins** |
| [#11](https://github.com/fiztoz/uptime-phoenix/pull/11) | `golang` image | `1.25-alpine` → `1.26-alpine` | Docker | Medium–high | Align with `go.mod` / `GOTOOLCHAIN` first |
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
| `mongo-driver/v2` | Database monitor (MongoDB) | `go test ./internal/adapters/checker/ -run Database` / Mongo paths |
| `grpc` | gRPC health checker | `go test ./internal/adapters/checker/ -run GRPC` |
| `client_golang` | Prometheus `/metrics` | `go test ./internal/adapters/metrics/...` + manual `/metrics` smoke |

**Files typically touched:** `go.mod`, `go.sum` only (unless APIs change).

### 3.2 Docker base images

| Image | Files | Notes |
|---|---|---|
| `golang:1.25-alpine` | `Dockerfile`, `Dockerfile.split` | Must stay consistent with `go.mod` (`go 1.25.12`) and workflow `GOTOOLCHAIN` |
| `node:22-alpine` | `Dockerfile`, `Dockerfile.split` | Frontend image build only; Bun installed inside |
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

**Important:** Dependabot PRs that only change `uses: org/action@vN` in comments or unpinned CI lines are incomplete for `release.yml`.  
Any upgrade of a **SHA-pinned** action must update:

```yaml
uses: org/action@<full-sha> # vX.Y.Z
```

Resolve SHA with:

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

**Do not land these as five separate merges.** Treat as one coordinated “Wave F” (below).

---

## 4. Execution waves

### Wave 0 — Hygiene (same day)

1. Rebase all open Dependabot PRs onto green `main`.
2. Confirm frontend failures from Prettier docs are gone on rebased PRs.
3. Optionally comment on major PRs: `@dependabot ignore this major version` **or** leave open but blocked until Wave F (preferred: ignore majors so the queue stays small).

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
- [ ] `go test -race ./internal/adapters/checker/...` (or full `go test -race ./...`) locally if desired  
- [ ] Merge (squash OK under branch protection)

### Wave B — Docker bases that stay on current majors (half day)

1. #7 nginx `1.27 → 1.31`  
   - Gate: CI `docker` job + local `docker compose -f docker-compose.split.yml build` if you use split mode  
2. #11 golang image `1.25 → 1.26`  
   - **Only after** deciding whether `go.mod` / `GOTOOLCHAIN` move to 1.26  
   - If keeping `go 1.25.12`, **close #11** and pin Dockerfile to `golang:1.25-alpine` intentionally  
3. #3 node `22 → 25` — **defer** (see Wave D)

### Wave C — GitHub Actions (half day, one PR preferred)

**Close Dependabot #2, #4, #6, #10, #14 as-is.**  
Replace with a **single manual PR**: `chore(ci): bump Actions used by CI/release`.

| Action | Target | Also update |
|---|---|---|
| `setup-qemu-action` | latest stable 4.x | SHA in `release.yml` (both jobs) |
| `upload-artifact` / `download-artifact` | compatible pair (read major migration notes) | SHAs in `release.yml` |
| `action-gh-release` | latest 2.x **or** 3.x after reading changelog | SHA in `release.yml` |
| `build-push-action` | v7 if still compatible | `ci.yml` |

**Validation:**

```bash
# Local
actionlint .github/workflows/*.yml   # or CI actionlint job

# CI must be green

# Optional release dry-run (no publish)
gh workflow run release.yml -f version=0.0.0-snapshot.deps -f skip_docker=true
```

### Wave D — Deferred base images

| PR | Decision |
|---|---|
| #3 node 22→25 | **Close / ignore major** until frontend Wave F or explicit need. Node 22 is LTS-class for the Dockerfile stage; Bun is what actually builds. |

### Wave F — Frontend majors (dedicated effort, 1–3 days)

**Close or ignore majors on PRs #15–#19.** Implement as **one branch**, not five Dependabot merges.

#### F.1 Target stack (proposed)

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

#### F.2 Zod 4 impact (code)

Search and migrate:

```bash
rg -n "from 'zod'|from \"zod\"|z\." web/src --glob '!**/paraglide/**'
```

Likely touch areas:

- Form schemas used with `sveltekit-superforms`
- API payload validation helpers
- Any `z.object` / `z.string()` refinements that changed error-map or default APIs in Zod 4

Follow [Zod 4 migration guide](https://zod.dev) (breaking changes around error formatting, `z.record`, defaults, etc.).

#### F.3 Frontend validation gate

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

#### F.4 Wave F exit criteria

- [ ] `bun run check` zero errors  
- [ ] Unit + e2e green  
- [ ] Docker web build still works (`USE_PREBUILT_WEB=1`)  
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
| Day 0 | Wave 0 | All PRs rebased; majors ignored or parked |
| Day 0–1 | Wave A | 4–5 Go patches on `main` |
| Day 1–2 | Wave B (nginx only) | Optional image bump |
| Day 2–3 | Wave C | One Actions pin PR; close Dependabot action majors |
| Later | Wave F | Coordinated frontend majors when you want the investment |
| Later | golang 1.26 / node 25 | Only with intentional toolchain upgrade |

---

## 7. Decision summary

| Bucket | PRs | Do |
|---|---|---|
| **Merge soon** | #5, #9, #12, #13, maybe #8 | Rebase → CI green → merge |
| **Merge carefully** | #7, #14 | Rebase → docker/CI green |
| **Replace with manual PR** | #2, #4, #6, #10 | Close Dependabot; one SHA-aware Actions PR |
| **Align toolchain first** | #11 | Match `go.mod` or close |
| **Defer / ignore major** | #3, #15, #16, #17, #18, #19 | Wave F or ignore |

---

## 8. Definition of done for this plan

- [ ] Wave A merged (or explicitly skipped with reason)  
- [ ] No stale red CI caused only by pre-Prettier `main`  
- [ ] Release workflow still SHA-pinned and actionlint-clean  
- [ ] Frontend majors either ignored or completed as Wave F  
- [ ] Dependabot config tuned so the next run is a small patch queue, not 18 majors  

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
