# Dependabot upgrade plan (review)

> **Created:** 2026-08-08  
> **Last reviewed:** 2026-08-09  
> **Repo:** [fiztoz/uptime-phoenix](https://github.com/fiztoz/uptime-phoenix)  
> **Open Dependabot PRs right now:** **0**

This document is the working plan and status board for the Dependabot flood after
the repo went public. It was first written when PRs **#2–#19** were open. Most of
that queue is already resolved; what remains is **validation debt** and a future
**frontend major** program (Wave F).

---

## 0. Status review (2026-08-09)

### 0.1 Remaining open PRs

| State | Count | Notes |
|---|---|---|
| Open Dependabot PRs | **0** | Queue cleared |
| Open other PRs | 0 (as of review) | — |

There is nothing left to rebase/merge from the original Dependabot batch.

### 0.2 What landed on `main`

| PR | Change | Outcome | Plan bucket (original) |
|---|---|---|---|
| #5 | pro-bing `0.9.0 → 0.9.1` | **Merged** | Wave A — safe Go |
| #9 | x/crypto `0.53 → 0.54` | **Merged** | Wave A — safe Go |
| #13 | grpc `1.82.1 → 1.83.0` | **Merged** | Wave A |
| #12 | mongo-driver `2.7 → 2.8` | **Merged** | Wave A (wanted success-path smoke) |
| #8 | client_golang `1.20.5 → 1.24.1` | **Merged** | Wave A medium |
| #14 | build-push-action `v6 → v7` | **Merged** | Wave C / CI |
| #7 | nginx `1.27 → 1.31` alpine | **Merged** | Wave B |
| #11 | golang image `1.25 → 1.26` alpine | **Merged** | Wave B (toolchain caution) |
| #2 | setup-qemu-action `3.6 → 4.2` (SHA pin) | **Merged** | Wave C release path |
| #4 | upload-artifact `4.6 → 7.0` (SHA pin) | **Merged** | Wave C release path |
| #10 | download-artifact `4.3 → 8.0` (SHA pin) | **Merged** | Wave C release path |
| #6 | action-gh-release `2.3 → 3.0` (SHA pin) | **Merged** | Wave C publish path |
| #20 | this plan doc | **Merged** | docs |

### 0.3 What was closed without merge (majors deferred)

| PR | Change | Outcome | Why that was right |
|---|---|---|---|
| #3 | node `22 → 25` alpine | **Closed** | Major base image; Docker frontend stage still Node 22 |
| #15 | `@sveltejs/vite-plugin-svelte` `5 → 7` | **Closed** | Must move with Vite major |
| #16 | `prettier-plugin-svelte` `3 → 4` | **Closed** | Tooling major; fine alone later but not urgent |
| #17 | `zod` `3 → 4` | **Closed** | Potentially breaking; see §5 |
| #18 | `vite` `6 → 8` | **Closed** | Coupled to Kit + plugin-svelte |
| #19 | `typescript` `5 → 7` | **Closed** | Large compiler jump; do stepwise |

Closing these majors instead of merging them one-by-one matches the plan’s Wave F
intent (even if they were closed rather than left open with “ignore major”).

### 0.4 Current tree (relevant pins)

| Surface | Current |
|---|---|
| `go.mod` | `go 1.25.12` (language minimum) |
| `Makefile` / CI / release `GOTOOLCHAIN` | `go1.25.12` |
| Docker Go builder | **`golang:1.26-alpine`** (release binaries built with Go 1.26) |
| Docker Node builder | `node:22-alpine` |
| Docker nginx (split web) | `nginx:1.31-alpine` |
| `release.yml` qemu | SHA → v4.2.0 |
| `release.yml` upload-artifact | SHA → v7.0.1 |
| `release.yml` download-artifact | SHA → v8.0.1 |
| `release.yml` gh-release | SHA → v3.0.2 |
| `ci.yml` build-push-action | `@v7` (floating major tag) |
| Frontend lock (approx) | TS 5.9.3, Vite 6.4.3, plugin-svelte 5.1.1, prettier-plugin-svelte 3.5.2, zod 3.25.76, Kit 2.66.0 |

---

## 1. Remaining work (not open PRs — validation debt)

Even with zero open Dependabot PRs, several merges skipped the **extra gates** the
original plan called for. Treat these as open checklist items on `main`.

### 1.1 High priority — release path after Actions majors

These merged and update **privileged** release steps. CI on `main` does **not** run
the full publish job.

| Item | Risk if broken | How to validate |
|---|---|---|
| upload-artifact v7 + download-artifact v8 | Dry-run artifact missing → publish attaches nothing / fails | `gh workflow run release.yml -f version=0.0.0-snapshot.deps -f skip_docker=true` then confirm artifact download works; ideally a full dry-run with Docker |
| setup-qemu-action v4 | Multi-arch dry-run/publish fails | Dispatch with `skip_docker=false` |
| action-gh-release v3 | Tag publish creates bad/missing GitHub Release | Only on tag-push + Environment `release` approval — use a **prerelease** tag e.g. `v0.0.0-rc.deps1` when ready |

**Definition of done for §1.1:** one successful release **dry-run** (dispatch) and,
when you next cut a real/prerelease tag, confirm GHCR + GitHub Release assets look right.

### 1.2 Medium — Go 1.26 builder vs go 1.25.12 module

`#11` raised the **Docker build image** to Go 1.26 while:

- `go.mod` still says `go 1.25.12`
- CI/Makefile still force `GOTOOLCHAIN=go1.25.12`

That is a valid “build with newer toolchain, support older language version” setup,
but it means **release binaries from Docker are not the same compiler as local/CI
default**. Documented intentional choice is fine; accidental drift is not.

| Option | When to pick |
|---|---|
| **A. Keep split** | Hobby OK: document “Docker release = Go 1.26; dev/CI = 1.25.12” |
| **B. Align up** | Bump `GOTOOLCHAIN` (+ optionally `go` directive) to 1.26 everywhere |
| **C. Align down** | Pin Docker back to `golang:1.25-alpine` if you want bit-identical compiler to CI |

**Suggested for hobby:** Option A short-term + run `go test` once under Go 1.26
locally before the next public tag. Option B when you care about one toolchain.

### 1.3 Medium — Mongo driver 2.8

`#12` merged without an explicit known-good MongoDB success-path smoke in the plan.
CI checker tests alone may only exercise bad-host / unit paths.

**Remaining:** if you use the database monitor with MongoDB in real deployments, run one
live `ping`/`select_1` against a throwaway Mongo after upgrade.

### 1.4 Low — nginx 1.31

Merged. Build is covered by CI docker job. Optional: start split web container and
`nginx -t` / hit `/` once.

### 1.5 Dependabot config still too noisy for next run

`.github/dependabot.yml` still allows major floods. **Remaining hygiene PR:**

- Group patch/minor updates
- Ignore semver-major for `typescript`, `vite`, `zod`, `@sveltejs/vite-plugin-svelte`, `prettier-plugin-svelte`, and ideally `node` Docker
- Lower `open-pull-requests-limit` (e.g. 3)

---

## 2. Review of major changes (Wave F) — deeper take

Majors were correctly **not** merged. Here is a sharper review of each closed PR
and whether/when to revisit.

### 2.1 TypeScript 5 → 7 (#19) — **do not jump straight to 7**

| | |
|---|---|
| Locked today | ~5.9.3 |
| Dependabot proposed | 7.0.2 |
| Coupled to | `typescript-eslint`, `svelte-check`, Svelte 5 types, Node types |

**Assessment:** High risk. TS 6/7 change checking behavior and may need eslint stack
upgrades. Prefer:

1. Stay on **5.9.x** (already current major line), or  
2. Planned hop **5 → 6** only when Svelte/eslint ecosystem docs say ready, then reassess 7.

**Do not** reopen #19 as-is.

### 2.2 Vite 6 → 8 (#18) + vite-plugin-svelte 5 → 7 (#15) — **must ship together**

| | |
|---|---|
| Locked today | Vite ~6.4.3, plugin-svelte ~5.1.1, Kit ~2.66 |
| Proposed | Vite 8.2, plugin-svelte 7.2 |

**Assessment:** These are one unit. Community migration notes (Vite 8 era) say
`@sveltejs/vite-plugin-svelte` major jumps with Vite 8, and Kit needs a new enough
2.x (e.g. 2.53+). Also update:

- `@sveltejs/kit` to a Vite-8-capable minor  
- adapters (`adapter-static` / `adapter-node`) if peers complain  
- possibly `@tailwindcss/vite`

**Order inside Wave F build-tool group:**

1. Read Kit changelog for minimum version supporting Vite 8  
2. Bump Kit + plugin-svelte + Vite in **one** PR  
3. `bun run check && bun run build` + Docker `USE_PREBUILT_WEB=0` and `=1`

Skipping intermediate Vite 7 is acceptable **if** Kit’s peer range allows Vite 8
directly; otherwise do 6→7→8.

### 2.3 prettier-plugin-svelte 3 → 4 (#16) — **lowest-risk major**

| | |
|---|---|
| Locked today | ~3.5.2 |
| Proposed | 4.1.1 |

**Assessment:** Tooling-only. Can be a small solo PR:

```bash
cd web && bun add -d prettier-plugin-svelte@^4
bun run format
bun run lint
```

Expect a formatting-only diff. Safe to do **before** the Vite/TS wave if you want a
quick win.

### 2.4 Zod 3 → 4 (#17) — **breaking, but low code surface in this repo**

| | |
|---|---|
| Locked today | zod 3.25.76 (direct dep) |
| Proposed | 4.4.3 |
| Direct `import` from `zod` in `web/src` | **None found** (review date) |
| Why it is still listed | `package.json` dependency; `sveltekit-superforms` peers `zod ^3 \|\| ^4` and may pull zod 4 optionally |

**Assessment:**

- **Code migration cost is currently low** if you truly have no `z.` schemas in app code.  
- Still a **product decision**: bumping the direct dependency changes what Superforms /
  future forms resolve.  
- Superforms 2.30 peers both Zod 3 and 4 — good.  
- Follow [Zod 4 migration guide](https://zod.dev/v4/changelog) if any schema appears later.

**Recommended:**

1. Confirm with `rg "from ['\"]zod['\"]|\\bz\\." web/src` before any bump  
2. If still unused: either **remove unused `zod` dep** or bump to 4 in a tiny PR after
   Superforms form smoke  
3. If you add forms later, target Zod 4 from the start

This is **easier than the plan originally assumed**, but still not free (lockfile +
peer resolution + Superforms runtime).

### 2.5 Node 22 → 25 Docker (#3) — **keep deferred**

| | |
|---|---|
| Locked today | `node:22-alpine` |
| Proposed | `node:25-alpine` |

**Assessment:** Dockerfile stage runs `npm run build` (Vite) under Node when
`USE_PREBUILT_WEB=0`. Node 22 is the right LTS-class default. Node 25 is current
line / non-LTS for long support windows.

**Revisit when:** Wave F forces a Node floor (Vite 8 needs Node ≥20.19 / 22.12 —
still satisfied by 22), or you drop Node 22 upstream images.

### 2.6 Wave F recommended shape (revised)

Do **not** open five Dependabot majors again. Prefer **2–3 human PRs**:

| PR | Scope | Gate |
|---|---|---|
| **F0** (optional) | prettier-plugin-svelte 4 | `bun run lint` / format |
| **F1** | Zod 4 **or** remove unused zod | `bun run check`, form smoke if any |
| **F2** | Vite 8 + plugin-svelte 7 + Kit bump | check, test, build, e2e, both Docker web modes |
| **F3** (later) | TypeScript 6 (not 7 first) + eslint | check + lint |

TypeScript 7 stays **out of scope** until 6 is stable in this repo.

### 2.7 Wave F validation gate (unchanged, still required)

```bash
cd web
bun install --frozen-lockfile
bun run check
bun run test
bun run build
bun run lint
bun run test:e2e   # or CI e2e

# Both Docker frontend paths
docker build --build-arg USE_PREBUILT_WEB=0 -t uptime-phoenix:web-source .
docker build --build-arg USE_PREBUILT_WEB=1 -t uptime-phoenix:web-prebuilt .
```

Repo: `make gate-full` before merge to `main`.

---

## 3. Original inventory (historical)

Kept for audit. Status column reflects 2026-08-09 reality.

| PR | Dependency | Proposed | Risk | Status |
|---|---|---|---|---|
| #5 | pro-bing | 0.9.1 | Low | **Merged** |
| #9 | x/crypto | 0.54.0 | Low | **Merged** |
| #12 | mongo-driver | 2.8.0 | Low–med | **Merged** — success-path smoke still optional debt |
| #13 | grpc | 1.83.0 | Low–med | **Merged** |
| #8 | client_golang | 1.24.1 | Medium | **Merged** |
| #14 | build-push-action | v7 | Medium | **Merged** |
| #7 | nginx image | 1.31-alpine | Medium | **Merged** |
| #2 | setup-qemu-action | 4.2.0 | Medium | **Merged** — exercise multi-arch dry-run |
| #6 | action-gh-release | 3.0.2 | Medium–high | **Merged** — needs real tag publish smoke |
| #4 | upload-artifact | 7.0.1 | High | **Merged** — pair with dry-run |
| #10 | download-artifact | 8.0.1 | High | **Merged** — only on publish job |
| #11 | golang image | 1.26-alpine | Medium–high | **Merged** — toolchain split vs GOTOOLCHAIN |
| #3 | node image | 25-alpine | High | **Closed** — deferred |
| #15 | vite-plugin-svelte | 7.x | High | **Closed** — Wave F2 |
| #16 | prettier-plugin-svelte | 4.x | High | **Closed** — Wave F0 optional |
| #17 | zod | 4.x | High | **Closed** — Wave F1 (low app surface) |
| #18 | vite | 8.x | High | **Closed** — Wave F2 |
| #19 | typescript | 7.x | High | **Closed** — do not reopen; prefer TS 6 later |

---

## 4. Affected surfaces (still accurate)

### 4.1 Go modules

| Package | Used for | Post-merge note |
|---|---|---|
| pro-bing | ping checker | Merged 0.9.1 |
| x/crypto | auth crypto | Merged 0.54 |
| mongo-driver/v2 | DB monitor Mongo | Merged 2.8 — optional live smoke |
| grpc | gRPC checker | Merged 1.83 |
| client_golang | Prometheus metrics | Merged 1.24 |

### 4.2 Docker

| Image | Files | Post-merge note |
|---|---|---|
| golang alpine | `Dockerfile`, `Dockerfile.split` | Now **1.26** |
| node alpine | same | Still **22** |
| nginx alpine | `Dockerfile.split` web | Now **1.31** |

### 4.3 Actions

| Action | Where | Pin style |
|---|---|---|
| setup-qemu | release.yml ×2 | SHA **v4.2.0** |
| upload-artifact | release.yml dry-run | SHA **v7.0.1** |
| download-artifact | release.yml publish | SHA **v8.0.1** |
| action-gh-release | release.yml publish | SHA **v3.0.2** |
| build-push-action | ci.yml | floating **@v7** |

---

## 5. Decision summary (remaining)

| Bucket | What | Do next |
|---|---|---|
| **Done** | Waves A/B + most of C as Dependabot merges | Nothing to merge |
| **Validate** | Release Actions v3/v4/v7/v8 | Dry-run dispatch; later prerelease tag |
| **Decide** | Go 1.26 Docker vs GOTOOLCHAIN 1.25.12 | Document or align |
| **Optional smoke** | Mongo 2.8, nginx 1.31 | Live checks if you use those paths |
| **Future majors** | Vite 8 stack, Zod 4, TS 6, prettier-plugin 4 | Wave F0–F3 human PRs; ignore Dependabot majors |
| **Hygiene** | dependabot.yml | Group + ignore majors so the flood does not return |

---

## 6. Definition of done (updated)

- [x] Dependabot queue #2–#19 resolved (merged or closed)  
- [x] Plan merged (#20)  
- [ ] Release dry-run green after Actions majors  
- [ ] Explicit decision recorded for Go 1.26 builder vs 1.25.12 GOTOOLCHAIN  
- [ ] Dependabot config tightened (ignore majors / group minors)  
- [ ] Wave F only when intentionally scheduled (not via surprise Dependabot)  

---

## 7. Quick links

| Resource | URL / path |
|---|---|
| Open PRs | https://github.com/fiztoz/uptime-phoenix/pulls |
| Dependabot config | `.github/dependabot.yml` |
| CI | `.github/workflows/ci.yml` |
| Release | `.github/workflows/release.yml` |
| Releasing docs | `docs/RELEASING.md` |
| Frontend package | `web/package.json` |
| Go modules | `go.mod` |
| Zod 4 changelog | https://zod.dev/v4/changelog |
