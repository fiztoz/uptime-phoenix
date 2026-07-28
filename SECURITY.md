# Security Policy

## Project status

Phoenix is a **hobby project** that is **not under active development**. There is no
security response team, no paid support, and **no SLA** for vulnerability reports.

Reports are welcome as a courtesy. Fixing them is best-effort only and may never happen.
If you need a maintained monitoring stack, evaluate actively supported alternatives or
fork this repository and maintain the fork yourself.

## Supported versions

No version is officially supported. The `main` branch is the only reference tree.

| Branch / tag | Support |
|---|---|
| `main` | Best-effort, no guarantees |
| Releases / tags (if any) | None |

## Reporting a vulnerability

**Please do not open a public GitHub issue for security-sensitive findings.**

Prefer one of:

1. **GitHub private vulnerability reporting** (Security → Report a vulnerability), if
   enabled on this repository.
2. Contact the repository owner via their GitHub profile for a private channel.

Include:

- Affected component (API, checker, notifier, Helm chart, frontend, …)
- Steps to reproduce
- Impact (auth bypass, secret leak, RCE, SSRF against cloud metadata, …)
- Whether a fix suggestion is included

You may get no reply. That is not an invitation to disclose recklessly against live
third-party deployments; it reflects the hobby status of this project.

## Threat model (read before deploying)

Phoenix is a **self-hosted** tool. The operator is trusted.

### Intentional capabilities that look like “attacks” in other products

These are **features**, not accidental holes, when used by an authenticated operator:

| Capability | Why it exists | Risk if abused |
|---|---|---|
| HTTP/TCP/DNS/… monitors | Probe real services | SSRF / internal network scanning from the Phoenix host |
| Notification webhooks | Alert external systems | Server-side request forgery to attacker-controlled URLs |
| Custom status-page CSS | Theming | Stored CSS injection on public status pages (admin-controlled) |
| Database / Docker / MQTT / etc. monitors | Health-check real infra | Requires credentials the operator supplies |

Compromising an **admin** account (or any account with broad grants) is effectively full
control of monitoring and outbound reach from the Phoenix process network.

### Defaults that are unsafe on the public internet

Local/dev convenience defaults must be changed before any exposure beyond localhost:

- Bootstrap user/password (documented as `admin` / `ChangeMe123!`)
- `JWT_SECRET` placeholder (`change-me-in-production` / compose `change_me_jwt`)
- Compose publishing MariaDB on host port `3306` with weak passwords
- `PRODUCTION=false` (dev CORS behaviour)

Recommended minimum for any shared deployment:

1. Strong unique `JWT_SECRET`, `BOOTSTRAP_PASSWORD`, and DB passwords
2. `PRODUCTION=true`
3. Do not publish database ports publicly
4. TLS termination (Ingress, reverse proxy, or Cloudflare Tunnel)
5. Restrict who can reach the admin UI; keep RBAC grants minimal

### What we try to avoid (and you should still verify)

- Returning secrets in public JSON (e.g. status-page password hashes)
- Open self-registration after the first user
- Unauthenticated `/metrics` (API-key middleware when wired)
- Wildcard CORS in production when `PRODUCTION=true` and no origins are configured

This list is **not** a guarantee. Audit the code you run.

## Scope notes for researchers

**In scope (interesting):** auth bypass, privilege escalation, secret leakage via API or
public status pages, remote code execution, dependency supply-chain issues in release
artifacts you actually build.

**Usually out of scope / accepted risk:** SSRF via monitor or webhook configuration by an
authenticated user; weak default secrets on a fresh local compose stack; denial of
service by flooding a self-hosted instance; issues that require already having admin
credentials.

## Prefer forking

If you plan to run Phoenix for real traffic or multi-tenant scenarios, **fork** the
project, add CI, pin and scan dependencies, enforce strong secrets at startup, and treat
your fork as the product of record.
