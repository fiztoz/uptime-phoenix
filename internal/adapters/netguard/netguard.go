// Package netguard keeps user-configured outbound connections (monitor checks
// and notification deliveries) away from destinations their owner may not
// reach.
//
// The guard runs as a net.Dialer control hook, so it sees the IP the socket is
// about to connect to after DNS resolution. That one check covers literal IPs,
// hostnames that resolve to internal addresses (including DNS rebinding), and
// redirects, because every hop dials through the same hook.
//
// Two tiers, chosen per check or delivery from the context (see ForContext and
// domain.OutboundTrust):
//
//   - The floor, which no tier and no allowlist can lift: link-local
//     (169.254.0.0/16, fe80::/10, which includes the 169.254.169.254 metadata
//     address), the other cloud metadata addresses (fd00:ec2::254,
//     100.100.100.200 and Azure's WireServer 168.63.129.16), multicast and
//     unspecified addresses, and their IPv4-mapped and NAT64 forms.
//   - Restricted (resources owned by a non-admin, a deleted user, or an owner
//     that could not be resolved, and any caller that attached no trust): the
//     floor, plus loopback, RFC 1918, CGNAT 100.64.0.0/10, IPv6 unique-local
//     and the reserved ranges, unless the destination is inside
//     OUTBOUND_ALLOWED_CIDRS.
//
// Admin-owned resources get the floor only, so an install where only admins
// configure monitors keeps reaching its private targets after upgrade.
// Remote probes run with TrustAllOwners: they are deployed by the operator
// into the networks they monitor, so they apply the floor only.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// EnvAllowedCIDRs names the environment variable holding the operator
// allowlist for restricted (non-admin-owned) monitors and notifications.
const EnvAllowedCIDRs = "OUTBOUND_ALLOWED_CIDRS"

// ErrBlocked reports a connection refused because its destination is not
// allowed for the owner of the monitor or notification.
var ErrBlocked = errors.New("destination address is not allowed")

// restrictedPrefixes are refused for restricted owners unless allowlisted.
// The floor (link-local, metadata, multicast, unspecified) is checked first
// and is not repeated here.
var restrictedPrefixes = mustPrefixes(
	"0.0.0.0/8",      // "this" network
	"10.0.0.0/8",     // RFC 1918
	"100.64.0.0/10",  // CGNAT
	"127.0.0.0/8",    // loopback
	"172.16.0.0/12",  // RFC 1918
	"192.0.0.0/24",   // IETF protocol assignments
	"192.168.0.0/16", // RFC 1918
	"240.0.0.0/4",    // reserved, limited broadcast
	"::1/128",        // loopback
	"64:ff9b:1::/48", // local-use NAT64
	"fc00::/7",       // unique-local
)

// nat64Prefix embeds an IPv4 address in its low 32 bits.
var nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")

// cloudMetadataAddrs are metadata or host-agent endpoints outside the
// link-local range.
var cloudMetadataAddrs = map[netip.Addr]bool{
	netip.MustParseAddr("fd00:ec2::254"):   true, // AWS IMDS over IPv6
	netip.MustParseAddr("100.100.100.200"): true, // Alibaba Cloud metadata
	netip.MustParseAddr("168.63.129.16"):   true, // Azure WireServer
}

// CheckFloor returns an error when addr is in the always-blocked floor:
// unspecified, multicast, link-local or a cloud metadata address. No policy
// or allowlist lifts it. probe.ValidateDestination uses the same floor for
// the probe's hub endpoint.
func CheckFloor(addr netip.Addr) error {
	addr = addr.Unmap().WithZone("")
	if !addr.IsValid() {
		return errors.New("invalid IP address")
	}
	if addr.IsUnspecified() {
		return errors.New("unspecified IP address destination is forbidden")
	}
	if addr.IsMulticast() {
		return errors.New("multicast IP address destination is forbidden")
	}
	if addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return errors.New("link-local IP address destination is forbidden")
	}
	if cloudMetadataAddrs[addr] {
		return errors.New("cloud metadata destination is forbidden")
	}
	if nat64Prefix.Contains(addr) {
		if err := CheckFloor(embeddedIPv4(addr)); err != nil {
			return fmt.Errorf("NAT64 form of a forbidden destination: %w", err)
		}
	}
	return nil
}

func embeddedIPv4(addr netip.Addr) netip.Addr {
	b := addr.As16()
	return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
}

// Policy decides which destination addresses one tier may reach.
type Policy struct {
	allowed []netip.Prefix
	trusted bool
	shared  func() *sharedTransport
}

// ParseAllowlist parses a comma-separated list of CIDRs or bare IPs.
// IPv4-mapped IPv6 entries (::ffff:10.0.0.0/104) are converted to the IPv4
// prefix they describe, because destinations are compared in unmapped form.
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
			if p.Addr().Is4In6() {
				if p.Bits() < 96 {
					return nil, fmt.Errorf("%s: IPv4-mapped CIDR %q must be /96 or longer; list the IPv4 range instead", EnvAllowedCIDRs, part)
				}
				p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid address %q: %w", EnvAllowedCIDRs, part, err)
		}
		a = a.Unmap().WithZone("")
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// NewPolicy returns a restricted policy that also allows the given prefixes.
// The floor still applies inside them.
func NewPolicy(allowed []netip.Prefix) *Policy {
	p := &Policy{allowed: append([]netip.Prefix(nil), allowed...)}
	p.shared = sync.OnceValue(func() *sharedTransport { return newSharedTransport(p) })
	return p
}

// NewTrustedPolicy returns the admin tier: only the floor applies.
func NewTrustedPolicy() *Policy {
	p := &Policy{trusted: true}
	p.shared = sync.OnceValue(func() *sharedTransport { return newSharedTransport(p) })
	return p
}

// Trusted reports whether this is the admin tier (floor only).
func (p *Policy) Trusted() bool { return p.trusted }

// Check returns an ErrBlocked error when addr may not be reached.
func (p *Policy) Check(addr netip.Addr) error {
	addr = addr.Unmap().WithZone("")
	if err := CheckFloor(addr); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrBlocked, addr, err)
	}
	if p.trusted {
		return nil
	}
	if p.allowlisted(addr) {
		return nil
	}
	if nat64Prefix.Contains(addr) {
		if v4 := embeddedIPv4(addr); !p.allowlisted(v4) && inRestricted(v4) {
			return p.restrictedErr(addr)
		}
	}
	if inRestricted(addr) {
		return p.restrictedErr(addr)
	}
	return nil
}

// Allowed reports whether a connection to addr may proceed.
func (p *Policy) Allowed(addr netip.Addr) bool { return p.Check(addr) == nil }

func (p *Policy) allowlisted(addr netip.Addr) bool {
	for _, pre := range p.allowed {
		if pre.Contains(addr) {
			return true
		}
	}
	return false
}

func inRestricted(addr netip.Addr) bool {
	for _, pre := range restrictedPrefixes {
		if pre.Contains(addr) {
			return true
		}
	}
	return false
}

func (p *Policy) restrictedErr(addr netip.Addr) error {
	return fmt.Errorf("%w: %s is an internal address; only monitors and notifications owned by an admin may reach it unless an operator lists it in %s", ErrBlocked, addr, EnvAllowedCIDRs)
}

// Control is a net.Dialer Control hook enforcing the policy. Unix sockets are
// local host access: the admin tier may use them, the restricted tier may not.
func (p *Policy) Control(network, address string, _ syscall.RawConn) error {
	if strings.HasPrefix(network, "unix") {
		if p.trusted {
			return nil
		}
		return fmt.Errorf("%w: unix sockets are limited to admin-owned monitors", ErrBlocked)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %q is not an IP address", ErrBlocked, host)
	}
	return p.Check(addr)
}

// ControlContext is the context-aware form of Control for net.Dialer.
func (p *Policy) ControlContext(_ context.Context, network, address string, c syscall.RawConn) error {
	return p.Control(network, address, c)
}

// Dialer returns a dialer that enforces the policy.
func (p *Policy) Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, ControlContext: p.ControlContext}
}

// DialContext dials through a guarded dialer.
func (p *Policy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return p.Dialer(30*time.Second).DialContext(ctx, network, address)
}

// CheckHost resolves host (an IP literal or a name) and returns an
// ErrBlocked error when any address it resolves to may not be reached. Use it
// only where the connection itself cannot be guarded (a proxied request, where
// the proxy dials the target): it is a pre-check with a rebinding window, not
// a substitute for the dial hook. A name that does not resolve is refused for
// the restricted tier (the destination cannot be verified) and allowed for
// the admin tier (the proxy may resolve names this host cannot).
func (p *Policy) CheckHost(ctx context.Context, host string) error {
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrBlocked)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return p.Check(addr)
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		if p.trusted {
			return nil
		}
		return fmt.Errorf("%w: cannot verify %q before proxying it: %v", ErrBlocked, host, err)
	}
	for _, addr := range addrs {
		if err := p.Check(addr); err != nil {
			return fmt.Errorf("%s resolves to a destination that is not allowed: %w", host, err)
		}
	}
	return nil
}

var (
	restrictedPolicy atomic.Pointer[Policy]
	trustedPolicy    = NewTrustedPolicy()
	trustAllOwners   atomic.Bool
)

// Restricted returns the process's restricted policy, built from
// OUTBOUND_ALLOWED_CIDRS. An unparsable value falls back to the empty
// allowlist (everything internal blocked); call Validate at startup to fail
// fast instead.
func Restricted() *Policy {
	if p := restrictedPolicy.Load(); p != nil {
		return p
	}
	allowed, err := ParseAllowlist(os.Getenv(EnvAllowedCIDRs))
	if err != nil {
		allowed = nil
	}
	restrictedPolicy.CompareAndSwap(nil, NewPolicy(allowed))
	return restrictedPolicy.Load()
}

// Trusted returns the process's admin-tier policy (floor only).
func Trusted() *Policy { return trustedPolicy }

// ForContext returns the policy for the check or delivery ctx scopes: the
// admin tier when the probe process trusts every owner or when ctx carries
// domain.OutboundTrustAdmin or domain.OutboundTrustProbe, and the restricted
// tier otherwise, including when nothing was attached.
func ForContext(ctx context.Context) *Policy {
	if trustAllOwners.Load() {
		return Trusted()
	}
	switch domain.OutboundTrustFrom(ctx) {
	case domain.OutboundTrustAdmin, domain.OutboundTrustProbe:
		return Trusted()
	default:
		return Restricted()
	}
}

// TrustAllOwners makes every check and delivery in this process use the
// admin tier (floor only). The remote probe calls it at startup: it is
// deployed by the operator into the network it monitors, the hub decides who
// may assign checks to it, and it has no view of monitor ownership.
func TrustAllOwners() { trustAllOwners.Store(true) }

// Validate parses OUTBOUND_ALLOWED_CIDRS and returns any error.
func Validate() error {
	_, err := ParseAllowlist(os.Getenv(EnvAllowedCIDRs))
	return err
}

// AllowlistSize returns the number of entries in OUTBOUND_ALLOWED_CIDRS.
func AllowlistSize() int { return len(Restricted().allowed) }

// SetRestricted replaces the process's restricted policy. Tests use it;
// deployments configure the allowlist with OUTBOUND_ALLOWED_CIDRS.
func SetRestricted(p *Policy) { restrictedPolicy.Store(p) }

// AllowLoopbackForTests lets restricted-tier tests reach httptest servers on
// loopback, as an operator would by allowlisting it.
func AllowLoopbackForTests() {
	SetRestricted(NewPolicy(mustPrefixes("127.0.0.0/8", "::1/128")))
}

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}
