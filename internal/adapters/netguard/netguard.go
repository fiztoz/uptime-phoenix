// Package netguard keeps user-configured outbound connections (monitor checks
// and notification deliveries) away from internal networks.
//
// The guard runs as a net.Dialer Control hook, so it sees the IP the socket is
// about to connect to after DNS resolution. That one check covers literal IPs,
// hostnames that resolve to internal addresses (including DNS rebinding), and
// redirects, because every hop dials through the same hook.
//
// Blocked by default: loopback, RFC 1918 private, link-local (including the
// 169.254.169.254 metadata address), CGNAT 100.64.0.0/10, IPv6 unique-local,
// unspecified, multicast and broadcast addresses, and the IPv4-mapped and
// NAT64 forms of those. Operators who monitor internal targets on purpose list
// those networks in OUTBOUND_ALLOWED_CIDRS (comma-separated CIDRs or IPs).
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// EnvAllowedCIDRs names the environment variable holding the operator
// allowlist.
const EnvAllowedCIDRs = "OUTBOUND_ALLOWED_CIDRS"

// ErrBlocked reports a connection refused because its destination is an
// internal address that is not on the allowlist.
var ErrBlocked = errors.New("destination address is not allowed")

// blockedPrefixes are refused unless allowlisted.
var blockedPrefixes = mustPrefixes(
	"0.0.0.0/8",      // "this" network, unspecified
	"10.0.0.0/8",     // RFC 1918
	"100.64.0.0/10",  // CGNAT
	"127.0.0.0/8",    // loopback
	"169.254.0.0/16", // link-local, cloud metadata
	"172.16.0.0/12",  // RFC 1918
	"192.0.0.0/24",   // IETF protocol assignments
	"192.168.0.0/16", // RFC 1918
	"224.0.0.0/4",    // multicast
	"240.0.0.0/4",    // reserved, broadcast
	"::/128",         // unspecified
	"::1/128",        // loopback
	"64:ff9b:1::/48", // local-use NAT64
	"fc00::/7",       // unique-local
	"fe80::/10",      // link-local
	"ff00::/8",       // multicast
)

// nat64Prefix embeds an IPv4 address in its low 32 bits.
var nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")

// Policy decides which destination addresses are allowed.
type Policy struct {
	allowed []netip.Prefix
	shared  func() *http.Transport
}

// ParseAllowlist parses a comma-separated list of CIDRs or bare IPs.
func ParseAllowlist(raw string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			p, err := netip.ParsePrefix(part)
			if err != nil {
				return nil, fmt.Errorf("%s: invalid CIDR %q: %w", EnvAllowedCIDRs, part, err)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid address %q: %w", EnvAllowedCIDRs, part, err)
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}

// NewPolicy returns a policy that also allows the given prefixes.
func NewPolicy(allowed []netip.Prefix) *Policy {
	p := &Policy{allowed: append([]netip.Prefix(nil), allowed...)}
	p.shared = sync.OnceValue(p.Transport)
	return p
}

// Allowed reports whether a connection to addr may proceed.
func (p *Policy) Allowed(addr netip.Addr) bool {
	addr = addr.Unmap().WithZone("")
	for _, pre := range p.allowed {
		if pre.Contains(addr) {
			return true
		}
	}
	if nat64Prefix.Contains(addr) {
		b := addr.As16()
		if !p.Allowed(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})) {
			return false
		}
	}
	for _, pre := range blockedPrefixes {
		if pre.Contains(addr) {
			return false
		}
	}
	return true
}

// Control is a net.Dialer Control hook enforcing the policy.
func (p *Policy) Control(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %q is not an IP address", ErrBlocked, host)
	}
	if !p.Allowed(addr) {
		return fmt.Errorf("%w: %s is an internal address; an operator can allow it with %s", ErrBlocked, addr.Unmap().WithZone(""), EnvAllowedCIDRs)
	}
	return nil
}

// Dialer returns a dialer that enforces the policy.
func (p *Policy) Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, Control: p.Control}
}

// DialContext dials through a guarded dialer.
func (p *Policy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return p.Dialer(30*time.Second).DialContext(ctx, network, address)
}

// Transport returns an http.Transport, cloned from the stdlib defaults, whose
// connections are guarded. It keeps the stdlib's HTTP(S)_PROXY handling; with
// an environment proxy the guarded dial is to the proxy, so the proxy's
// address must be allowlisted and the proxy owns egress filtering.
func (p *Policy) Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = p.DialContext
	return t
}

// SharedTransport returns one pooled guarded transport per policy. Callers
// that need to change TLS or proxy settings must use Transport instead.
func (p *Policy) SharedTransport() *http.Transport { return p.shared() }

// Client returns an http.Client on the shared guarded transport.
func (p *Policy) Client(timeout time.Duration) *http.Client {
	return &http.Client{Transport: p.SharedTransport(), Timeout: timeout}
}

var defaultPolicy atomic.Pointer[Policy]

// Default returns the process policy built from OUTBOUND_ALLOWED_CIDRS. An
// unparsable value falls back to the empty allowlist (everything internal
// blocked); call Validate at startup to fail fast instead.
func Default() *Policy {
	if p := defaultPolicy.Load(); p != nil {
		return p
	}
	allowed, err := ParseAllowlist(os.Getenv(EnvAllowedCIDRs))
	if err != nil {
		allowed = nil
	}
	defaultPolicy.CompareAndSwap(nil, NewPolicy(allowed))
	return defaultPolicy.Load()
}

// Validate parses OUTBOUND_ALLOWED_CIDRS and returns any error.
func Validate() error {
	_, err := ParseAllowlist(os.Getenv(EnvAllowedCIDRs))
	return err
}

// SetDefault replaces the process policy. Tests use it to reach their
// loopback servers; deployments configure the guard with
// OUTBOUND_ALLOWED_CIDRS.
func SetDefault(p *Policy) { defaultPolicy.Store(p) }

// AllowLoopbackForTests lets tests reach httptest servers on loopback.
func AllowLoopbackForTests() {
	SetDefault(NewPolicy(mustPrefixes("127.0.0.0/8", "::1/128")))
}

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}
