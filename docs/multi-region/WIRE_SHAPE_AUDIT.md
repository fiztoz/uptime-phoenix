# Wire-shape and secret-exposure audit (AGENTS.md rule 5)

Recorded 2026-09-24 against `056176b` on `codex/multi-region-probe-plan`.
Companion to the [timezone/UTC audit](SECURITY_AUDIT.md); same discipline — every
status below carries a command that actually ran.

## Scope and method

The whole HTTP surface, enumerated by AST rather than by grep: a throwaway
`go/ast` + `go/printer` program walked `internal/adapters/http/**/*.go` (excluding
`_test.go`), resolved the second argument of every `c.JSON(status, payload)` call
to its parsed source text, and classified it. This reads the actual argument
expression tree, so the classification does not depend on line formatting or
naming luck.

**319 `c.JSON` payload sites** classified:

| Shape | Count | Meaning |
|---|---:|---|
| FUNC-CALL | 219 | payload produced by a call, e.g. `toXView(...)` |
| COMPOSITE-LIT | 51 | typed literal, e.g. `&someView{...}` |
| VIEW-NAMED | 27 | identifier whose name contains `view` |
| BARE-VAR | 15 | local variable, type not resolved by the scanner |
| UNTYPED-MAP | 6 | inline `map[string]any` — rule 5 deviation |
| DOMAIN | 1 | expression mentioning `domain.` |

The scan is a *triage* instrument, not a verdict: it says which payloads have no
compile-time field list, which is exactly the condition under which the historical
leak happened. Headline result: **this class is substantially healthy.** No shipped
leak was found, unlike the timezone class.

## Deviation: 6 endpoints return `map[string]any`

| Site | Payload |
|---|---|
| `handlers/auth.go:206` `Register` | `{"user": toUserView(user), "token": ""}` |
| `handlers/auth.go:365` `Me` | `{"user": toUserView(user)}` |
| `handlers/user.go:216` `Create` | `{"user": toUserView(user)}` |
| `handlers/user.go:242` `GetByID` | `{"user": toUserView(user)}` |
| `handlers/user.go:269` `Update` | `{"user": toUserView(user)}` |
| `handlers/configascode.go:57` `Validate` | `{"valid": len(errs) == 0, "errors": errs}` |

**No secret leaks at any of these today** — each wraps `toUserView`, which is a
proper View. The finding is structural, and it is the reason rule 5 exists: an
untyped map has no field list, so adding a field to it is invisible to the compiler
and to review. Rule 5's own history is a `PasswordHash` reaching an unauthenticated
endpoint through exactly this kind of "we'll just return this data" path.

`auth.go:206` carries a second, sharper smell: `"token": ""`. A permanently empty
token field is the sibling of rule 7's never-leave-a-stub-that-returns-success — a
wire slot that advertises a capability and silently delivers nothing. A client
reading `resp.token` gets `""` rather than an error.

`configascode.go:57` is the weakest case for change: `{"valid", "errors"}` is a
genuine ad-hoc result, but `errors` is `[]error`-ish and its serialized shape is
unspecified, so frontend/backend agreement there is by convention only (rule 5's
"do not guess field names" hazard).

## Accepted-risk exceptions — verified deliberate, not overlooked

These are *not* findings. Both carry written rationale, and I confirmed the
controls they claim.

**`GET /api/backup/export` returns bcrypt hashes, tokens and passwords.**
`BackupDocument` includes `BackupStatusPage.PasswordHash` (`backup_service.go:183`,
populated at `:631`/`:1072`) plus notification provider tokens/webhook URLs and
proxy passwords. `backup.go:25-29` states a "SECRETS POLICY (deliberate exception
to never return secrets)" and `backup_service.go:22-28` gives the reason: a
restorable backup must round-trip these or a protected status page stops being
protected after restore; Uptime Kuma does the same. Claimed controls verified:
`Cache-Control: no-store` is set (`backup.go:43`). Residual risk is accepted and
documented, and an offline crack of a bcrypt hash is the honest cost of
restorability. Not flagged for change.

**`POST /api/auth/setup-2fa` returns a raw TOTP secret.** The highest-stakes item in
the set — a leaked TOTP secret defeats 2FA outright. It is correctly confined:
`router.go:117` builds `protectedGroup := e.Group("/api/auth",
middleware.AuthMiddleware(authSvc))` and `:119` registers `setup-2fa` there,
*not* on the public `authGroup` (`:101-113`, which holds `has-users`, `register`,
`login`, `verify-2fa`, public WebAuthn and OIDC). It is also the caller's own secret,
during enrolment, where displaying it is the entire function. Correct as designed.

`configascode.go`'s `plan`/`res` and `backup.go:65`'s `summary` were **not** resolved
to types, so nothing here vouches for them. A config-as-code plan document is a
plausible place for provider configs to surface, and it is the obvious next thing to
check.

**`notification_template.go:280` — the single `domain.` hit — is benign.** The
expression is `domain.NotificationTemplateVariables()`, a `[]string` of template
variable *names*. No struct is serialized, so nothing can leak. It is also an
untyped map (`map[string][]string`), i.e. same structural class as the 6 above but
with no user data behind it.

## Not verified

Stated so the next reader does not mistake triage for proof.

- **The 270 FUNC-CALL and COMPOSITE-LIT sites were shape-classified, not
  field-inspected.** A View struct that includes a secret field would pass this
  scan unnoticed. Verifying those needs field-level inspection of each View type.
- **13 of the 15 BARE-VAR sites are unresolved.** Only two were traced by hand and
  both resolved clean: `backup.go:44` `doc` → `services.BackupDocument` (inspected
  as its own item, below) and `auth.go:285` `Setup2FAResponse` (route gating
  checked, below). The remaining 13 — including config-as-code's `plan` and `res`
  and backup-import's `summary` — were *named* in the triage output but never type
  resolved. An earlier draft of this document claimed four had been traced; that
  was wrong, and the correction is recorded here rather than quietly made, because
  overstating verification coverage is the precise failure this audit exists to
  catch.
- Read-only pass. **No test was written or executed for this class**, and no
  production behavior was characterized at runtime — this is static triage plus
  targeted reading.

## Suggested follow-up, in priority order

1. Convert the 5 `{"user": toUserView(user)}`-shaped sites to typed Views and
   resolve the `"token": ""` placeholder into an explicit contract.
2. Type-resolve `configascode.go`'s `plan`/`res` and `backup.go:65` `summary`, then
   the rest of the unresolved BARE-VAR set.
3. Field-inspect the View types behind the 270 typed sites for secret fields.
4. Consider whether `Validate`'s `errors` payload wants a defined shape, since the
   frontend must match its field names exactly.
