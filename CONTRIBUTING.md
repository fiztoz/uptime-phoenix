# Contributing to Uptime Phoenix

Thank you for contributing to Uptime Phoenix! This guide applies to **both human contributors and AI coding agents**.

> **This is a hobby repository and is not under active development.** Issues and PRs may
> never be reviewed. If you need ongoing maintenance, **fork** the project and work on
> your fork. See the README status banner and [SECURITY.md](./SECURITY.md).

> **All agents (AI or human) must read [`AGENTS.md`](./AGENTS.md) before writing any code.**
> It contains the non-negotiable architecture rules, locked tech stack, and file placement rules.
> This document is a shorter summary for humans; `AGENTS.md` is the canonical source.

---

## Quick Start

```bash
# Clone and enter
git clone https://github.com/fiztoz/uptime-phoenix.git && cd uptime-phoenix

# Local development (requires Go 1.25+ and Bun 1.0+ — never npm)
make dev          # all-in-one SQLite + hot reload
make dev-split    # MariaDB + Redis + API hot reload + Vite + worker

# Or with Docker
docker compose up # starts app + MariaDB

# Before pushing (CI runs on PR/main; still run the local gate)
make gate-fast    # build + vet + tests + web check
make gate-full    # full pre-merge gate when you can afford it
```

## Architecture in 30 Seconds

Uptime Phoenix uses **Port-and-Adapter (Hexagonal) architecture**:

```
cmd/ ──▶ adapters/ ──▶ core/services/ ──▶ core/ports/ ──▶ core/domain/
           │                                              (pure types)
           └── implements core/ports/* interfaces
```

- **`internal/core/`** — domain types, port interfaces, use cases. **No framework imports allowed here.**
- **`internal/adapters/`** — implementations of ports (HTTP handlers, DB repositories, monitor checkers, notification senders, WebSocket hub, auth, etc.)
- **`cmd/`** — composition root that wires adapters to services.

**The golden rule:** dependencies point inward. Adapters depend on core. Core never depends on adapters.

## Locked Tech Stack

Don't substitute these without discussion:

| Layer | Choice |
|---|---|
| Backend | Go 1.25+ |
| Frontend | Svelte 5 + SvelteKit + **Bun** |
| Database | MariaDB (primary), SQLite (dev/edge) |
| HTTP | Echo v4 |
| WebSocket | `coder/websocket` |
| Query builder | Bun |
| i18n | `inlang/paraglide-js` |
| Charts | LayerCake |
| UI primitives | shadcn-svelte |
| Deployment | Helm chart, single-pod default |

See `AGENTS.md` for the full list with "do NOT substitute" alternatives.

## Minimal-Dependency Principle

The **default deployment** must work with **zero external dependencies**:
- Single pod, MariaDB on PVC, embedded frontend, in-process EventBus.
- Redis, external DB, and separate web tier are **opt-in via Helm values**.

If your change requires Redis or an external service to boot, it breaks the default deployment. Don't do that.

## Adding a Monitor Type

1. Create `internal/adapters/checker/<type>.go` implementing `ports.Checker`.
2. Add one line to `internal/adapters/checker/registry.go`: `Register(YourChecker{})`.
3. Write tests in `internal/adapters/checker/<type>_test.go`.
4. That's it. No other files change.

See `AGENTS.md` for the approved monitor type list and architecture rules.

## Adding a Notification Provider

1. Create `internal/adapters/notifier/<provider>.go` implementing `ports.NotificationSender`.
2. Add one line to `internal/adapters/notifier/registry.go`: `Register(YourSender{})`.
3. Write tests with a mock HTTP server in `internal/adapters/notifier/<provider>_test.go`.
4. That's it.

See `AGENTS.md` for the approved notification provider list.

## Code Style

### Go
- `golangci-lint` must pass with zero warnings.
- `context.Context` as first parameter for all I/O functions.
- No `panic()` in service or adapter code — return errors.
- Use `log/slog` for logging, never `fmt.Println`.
- All libraries must be CGO-free (`CGO_ENABLED=0`).

### Svelte / TypeScript
- Svelte 5 runes only (`$state`, `$derived`, `$effect`, `$props`). No Svelte 4 `$:` syntax.
- Runes in modules use `.svelte.ts` extension.
- No `any` without a comment.
- `eslint` + `prettier` must pass.

## Before You Push

- [ ] `golangci-lint run` — zero warnings
- [ ] `go test ./...` — all pass
- [ ] `cd web && bun run lint` — passes (if frontend touched)
- [ ] `cd web && bun run build` — succeeds (if frontend touched)
- [ ] No framework/driver imports in `internal/core/`
- [ ] No new external dependency without checking CGO-free + updating `docs/ARCHITECTURE.md`
- [ ] DB migrations have both `.up.sql` and `.down.sql` (if DB touched)
- [ ] `helm lint charts/phoenix` passes (if Helm chart touched)

## Commit Messages

```
<type>(<scope>): <description>
```

**Types:** `feat`, `fix`, `refactor`, `test`, `docs`, `chore`, `ci`, `perf`
**Scopes:** `core`, `checker`, `notifier`, `http`, `ws`, `db`, `web`, `helm`, `auth`, `monitor`, `status-page`, `i18n`

Examples:
- `feat(checker): add MQTT broker monitor type`
- `fix(ws): handle reconnection after server restart`
- `docs(architecture): update notification provider list`

## Reference Documents

- [`AGENTS.md`](./AGENTS.md) — full rules (read this)
- [`docs/PLAN.md`](./docs/PLAN.md) — project goal and scope
- [`docs/ROADMAP.md`](./docs/ROADMAP.md) — delivery timeline
- [`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md) — detailed technical design (the source of truth)

## Questions?

Open an issue or check the design docs. The architecture is deliberate — when in doubt, refer to `AGENTS.md` and `docs/ARCHITECTURE.md`.
