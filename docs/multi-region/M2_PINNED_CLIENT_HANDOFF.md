# M2 Pinned TLS Client Slice Handoff

## 1. Scope & File Ownership

This handoff covers the bounded transport security slice for Milestone 2 as specified in `docs/multi-region/M2_WORK_CONTRACT.md` and `docs/multi-region/M2_PINNED_CLIENT_REVIEW.md`.

Owned and created files:
- `internal/adapters/probe/pinned_client.go`
- `internal/adapters/probe/pinned_client_test.go`
- `docs/multi-region/M2_PINNED_CLIENT_HANDOFF.md`

No other existing files were modified. No new modules were added. No commits, pushes, or deployments were performed.

---

## 2. Frozen Contract Implementation

Package `probe` exports:

```go
type EndpointPolicy struct {
    AllowedCIDRs []netip.Prefix
}

func ValidateDestination(addr netip.Addr, policy EndpointPolicy) error

func NewPinnedHTTPClient(endpoint, fingerprint string, policy EndpointPolicy) (*http.Client, error)
```

### Core Architecture & Invariants
1. **Endpoint Validation**:
   - Requires absolute `wss` URL targeting exactly `/ws/probe/v1` or `/ws/probe/enroll/v1`.
   - Hostname and valid 16-bit port (default 443).
   - Fails closed on userinfo, query parameters, URL fragments, trailing slashes, and encoded path variants (e.g. `/ws%2fprobe/v1`).
   - Redacts userinfo/secrets on malformed syntax without echoing raw input in errors.
2. **Fingerprint Verification**:
   - Requires exactly 64 lowercase hexadecimal characters representing the SHA-256 digest of the leaf certificate DER bytes.
   - Rejects uppercase hex, non-hex, separators, empty strings, and invalid lengths.
   - Verified in constant time via `crypto/subtle.ConstantTimeCompare`.
3. **TLS 1.3 Enforcement**:
   - `MinVersion = tls.VersionTLS13`, `MaxVersion = tls.VersionTLS13`.
   - Mandatory `VerifyPeerCertificate` hook checking leaf certificate presence, `NotBefore`, `NotAfter`, and constant-time fingerprint match.
   - Refuses TLS 1.2 or lower.
4. **Destination & DNS Policy (`ValidateDestination`)**:
   - Unspecified (`0.0.0.0`, `::`), multicast (`224.0.0.0/4`, `ff00::/8`), link-local (`169.254.0.0/16`, `fe80::/10`), and cloud metadata (`169.254.169.254`, `fd00:ec2::254`, `100.100.100.200`) destinations are forbidden under all conditions, even if included in `AllowedCIDRs`.
   - All addresses are normalized via `netip.Addr.Unmap()` to prevent IPv4-mapped IPv6 bypasses.
   - If `AllowedCIDRs` is empty, only public unicast addresses are permitted; private (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7`), loopback (`127.0.0.0/8`, `::1`), and CGNAT (`100.64.0.0/10`) require an explicit matching CIDR prefix.
   - `AllowedCIDRs` is defensively cloned at construction time so external slice mutations cannot broaden an active client's network authorization.
5. **Dialer & RoundTripper Guarantees**:
   - Environment proxies disabled (`Proxy = nil`).
   - Context-aware DNS resolution with a shared 10-second timeout across resolution and candidate socket connections, preserving caller cancellation.
   - Dials verified IP directly to prevent unvalidated second DNS lookups.
   - Redirects rejected via `CheckRedirect`.
   - Dedicated `RoundTripper` verifies request URL scheme (`https`), exact host, port, path, userinfo absence, query absence, fragment absence, encoded path absence, and rejects conflicting `Request.Host` headers.

---

## 3. Resolution of Review Findings

| Finding | Review Requirement | Resolution & Evidence |
|---|---|---|
| **1. Test Execution** | Use `rtk proxy env GOTOOLCHAIN=go1.26.6`, avoid rerunning for truncation, inspect exit code from log. | Ran single execution to local log, verified all subtests pass and command exited with code 0. |
| **2. Bounded Error Redaction** | `url.Parse` error wrapping can expose secrets in malformed URLs. | `NewPinnedHTTPClient` returns `invalid endpoint URL: malformed syntax` without wrapping `url.Parse` error. Tested with `wss://admin:secret-probe-token-to-redact@bad-host-url%\x00/ws/probe/v1`. |
| **3. Shared Dial Deadline** | Custom dial path lacked shared deadline across DNS resolution and socket candidates. | Derived `dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)` bounding DNS resolution and candidate dials, while preserving caller cancellation. |
| **4. Policy Slice Mutation** | `EndpointPolicy.AllowedCIDRs` captured caller slice reference. | Defensive copy created in `NewPinnedHTTPClient`. Regression test verifies mutating caller slice after construction does not alter client network authorization. |
| **5. RoundTripper Request Checks** | Dedicated RoundTripper must reject userinfo, encoded paths, and conflicting `Request.Host`. | Added checks in `pinnedRoundTripper.RoundTrip` for `req.URL.User != nil`, `req.URL.RawPath != ""`, `req.URL.EscapedPath() != req.URL.Path`, and conflicting `req.Host`. Unit tests exercise each case. |
| **6. Lint & Formatting** | Checked websocket closes, gofmt formatting, and import ordering. | Checked all websocket closes in test; `gofmt -l` clean; `go vet ./internal/adapters/probe/...` passed with zero warnings. |

---

## 4. Verification & Test Evidence

### Test Suite Execution
Command:
```bash
rtk proxy env GOTOOLCHAIN=go1.26.6 go test -v -race -count=1 ./internal/adapters/probe -run "^TestPinnedClient"
```

Result: `PASS` (exit code 0, duration 1.736s).

### Subtest Results Breakdown
1. **`TestPinnedClient_EndpointValidation`**:
   - `valid_runtime_endpoint`: PASS
   - `valid_enroll_endpoint_with_custom_port`: PASS
   - `valid_IP_endpoint`: PASS
   - `valid_IPv6_endpoint`: PASS
   - `reject_https_scheme`: PASS (rejected with "scheme must be wss")
   - `reject_ws_scheme`: PASS (rejected with "scheme must be wss")
   - `reject_userinfo`: PASS (rejected with "userinfo")
   - `reject_query_parameters`: PASS (rejected with "query")
   - `reject_fragment`: PASS (rejected with "fragment")
   - `reject_wrong_path`: PASS (rejected with "path must be /ws/probe/v1 or /ws/probe/enroll/v1")
   - `reject_trailing_slash`: PASS (rejected with "path must be /ws/probe/v1 or /ws/probe/enroll/v1")
   - `reject_root_path`: PASS (rejected with "path must be /ws/probe/v1 or /ws/probe/enroll/v1")
   - `reject_encoded_path_variant`: PASS (rejected with "encoded characters")
   - `reject_port_zero`: PASS (rejected with "valid 16-bit integer")
   - `reject_port_overflow`: PASS (rejected with "valid 16-bit integer")
   - `reject_invalid_port_string`: PASS (rejected with "malformed syntax")
   - `malformed_url_redacts_secrets_from_error`: PASS (verified `secret-probe-token-to-redact` not present in error)

2. **`TestPinnedClient_FingerprintValidation`**:
   - `valid_64-char_lowercase_hex`: PASS
   - `reject_uppercase_hex`: PASS
   - `reject_non-hex_characters`: PASS
   - `reject_colons_or_separators`: PASS
   - `reject_short_fingerprint`: PASS
   - `reject_empty_fingerprint`: PASS
   - `reject_long_fingerprint`: PASS

3. **`TestPinnedClient_PolicyPrefixValidation`**:
   - `valid_prefix`: PASS
   - `invalid_prefix_fails`: PASS
   - `caller_policy_mutation_after_construction_does_not_broaden_client_authorization`: PASS

4. **`TestPinnedClient_DestinationPolicyUnit`**:
   - `unspecified_IPv4_forbidden` (`0.0.0.0`): PASS
   - `unspecified_IPv6_forbidden` (`::`): PASS
   - `unspecified_forbidden_even_with_allowed_CIDR`: PASS
   - `multicast_IPv4_forbidden` (`224.0.0.1`): PASS
   - `multicast_IPv6_forbidden` (`ff02::1`): PASS
   - `multicast_forbidden_even_with_allowed_CIDR`: PASS
   - `link-local_IPv4_forbidden` (`169.254.1.1`): PASS
   - `link-local_IPv6_forbidden` (`fe80::1`): PASS
   - `link-local_forbidden_even_with_allowed_CIDR`: PASS
   - `cloud_metadata_169.254.169.254_forbidden`: PASS
   - `cloud_metadata_IPv6_fd00:ec2::254_forbidden`: PASS
   - `cloud_metadata_Alibaba_100.100.100.200_forbidden`: PASS
   - `mapped_IPv4_loopback_without_CIDR_forbidden` (`::ffff:127.0.0.1`): PASS
   - `mapped_IPv4_metadata_forbidden` (`::ffff:169.254.169.254`): PASS
   - `mapped_IPv4_private_without_CIDR_forbidden` (`::ffff:10.0.0.1`): PASS
   - `mapped_IPv4_multicast_forbidden` (`::ffff:224.0.0.1`): PASS
   - `mapped_IPv4_unspecified_forbidden` (`::ffff:0.0.0.0`): PASS
   - `loopback_127.0.0.1_without_CIDR_forbidden`: PASS
   - `loopback_::1_without_CIDR_forbidden`: PASS
   - `private_10.0.0.1_without_CIDR_forbidden`: PASS
   - `private_172.16.0.1_without_CIDR_forbidden`: PASS
   - `private_192.168.1.1_without_CIDR_forbidden`: PASS
   - `private_fc00::1_without_CIDR_forbidden`: PASS
   - `CGNAT_100.64.0.1_without_CIDR_forbidden`: PASS
   - `public_unicast_IPv4_permitted_without_CIDR` (`93.184.216.34`): PASS
   - `public_unicast_Cloudflare_DNS_permitted_without_CIDR` (`1.1.1.1`): PASS
   - `public_unicast_IPv6_permitted_without_CIDR`: PASS
   - `loopback_127.0.0.1_with_matching_CIDR_permitted`: PASS
   - `loopback_::1_with_matching_CIDR_permitted`: PASS
   - `private_10.1.2.3_with_matching_CIDR_permitted`: PASS
   - `private_192.168.1.50_with_non-matching_CIDR_forbidden`: PASS

5. **`TestPinnedClient_RealTLSServerScenarios`**:
   - `correct_pin_success_with_websocket_dial`: PASS (full `coder/websocket` handshake and text echo)
   - `wrong_pin_fails_handshake`: PASS (failed closed with "peer certificate fingerprint mismatch")
   - `expired_certificate_fails_handshake`: PASS (failed closed with "peer certificate has expired")
   - `not-yet-valid_certificate_fails_handshake`: PASS (failed closed with "peer certificate is not yet valid")
   - `TLS_1.2_refusal`: PASS (failed closed on TLS protocol version)
   - `redirect_rejection`: PASS (failed closed with "redirects are not allowed")
   - `caller_cancellation_honors_context`: PASS (failed with `context.Canceled`)
   - `loopback_connection_fails_without_explicit_CIDR`: PASS (destination policy rejection)
   - `client_reuse_prevention_across_host_port_and_path`: PASS (rejections verified for different host, port, path, query, userinfo, encoded path, and conflicting `Host` header)

---

## 5. Boundary Distinction for Subsequent M2 Slices

### Included in this slice:
- Pinned TLS 1.3 client constructor (`NewPinnedHTTPClient`).
- Leaf certificate SHA-256 constant-time fingerprint verification.
- Destination IP and CIDR policy evaluation (`ValidateDestination`).
- Strict URL and endpoint path parsing and validation.
- Context-aware dialer with shared 10s deadline and direct IP dialing.
- Dedicated `RoundTripper` locking requests to the pinned target and rejecting credential reuse across hosts, ports, paths, or conflicting `Host` headers.

### Excluded (to be implemented in subsequent M2 slices):
- Probe daemon CLI entrypoint and service bootstrap.
- Edge SQLite schema, migrations, and local state persistence.
- Single-use enrollment token generation, local storage, and exchange flow.
- WebSocket session supervisor, frame encoding/decoding, and channel management.
- Bounded one-reader / one-writer message loop with backoff and session fencing.
- Edge observation, intent outbox, and durable retry engine.
