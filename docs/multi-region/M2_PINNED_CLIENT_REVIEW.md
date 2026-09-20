# Integrator feedback: pinned TLS client slice

Keep Gemini 3.8 Flash and your three-file ownership. M2 is still in progress.
Do not edit the main integrator's M1 changes. Resolve this review before handoff.

1. Stop rerunning an unchanged test merely because the tool truncates output.
   Run once with output redirected to a local log, then inspect its final lines
   and actual exit code. Use `rtk proxy env GOTOOLCHAIN=go1.26.6` as the command
   prefix. Request only the specific local test permission, never a global bypass.
2. `NewPinnedHTTPClient` wraps `url.Parse` errors, which can include the entire
   rejected URL including userinfo/query credentials. Return a bounded redacted
   error and test malformed input containing a recognizable fake secret.
3. The custom dial path has a 10-second timer per socket attempt but no shared
   deadline around DNS resolution plus all candidate addresses. Derive one
   10-second context for the complete dial operation. Preserve caller cancellation.
4. `EndpointPolicy.AllowedCIDRs` is a caller-owned slice captured by the dialer.
   Clone it at construction so later mutation cannot broaden a live client's
   network authorization. Add a regression exercising mutation after construction.
5. The dedicated RoundTripper must also reject request URL userinfo, escaped path
   variants and a conflicting Request.Host. Constructor validation alone does not
   protect a reusable `http.Client`. Tests should attempt those request overrides.
6. Current lint reports unchecked websocket closes at pinned_client_test.go:576
   and :612, formatting in pinned_client.go, and import ordering in its test. Fix
   only your files; run focused lint (the integrator fixes the other files).

Teaching point from M1: a manually repaired test fixture is not production wiring;
a warm cache is not durable authority; enabling producer + consumer while leaving
legacy sends on creates two owners. The new real two-worker smoke now proves
single initial sends, escalation, ACK links, ACK cancellation, restart and recovery
on MariaDB. Keep every handoff tied to an exact function, failing case and observed
result; do not invent method names/routes or report a milestone percentage.
