package probe

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

var (
	cgnatPrefix       = netip.MustParsePrefix("100.64.0.0/10")
	cloudMetadataIPv6 = netip.MustParseAddr("fd00:ec2::254")
	cloudMetadataAli  = netip.MustParseAddr("100.100.100.200")

	specialNonPublicPrefixes = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT
		netip.MustParsePrefix("192.0.0.0/24"),    // IETF Protocol Assignments
		netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
		netip.MustParsePrefix("198.18.0.0/15"),   // Benchmarking
		netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
		netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
		netip.MustParsePrefix("240.0.0.0/4"),     // Reserved
		netip.MustParsePrefix("255.255.255.255/32"),
		netip.MustParsePrefix("2001:db8::/32"), // Documentation IPv6
		netip.MustParsePrefix("100::/64"),      // Discard prefix
	}
)

// EndpointPolicy specifies network destination constraints for probe connections.
type EndpointPolicy struct {
	AllowedCIDRs []netip.Prefix
}

// ValidateDestination verifies whether an address is allowed by the endpoint policy.
// Unspecified, multicast, link-local, and cloud metadata destinations are always forbidden,
// even if an allowed CIDR is supplied.
func ValidateDestination(addr netip.Addr, policy EndpointPolicy) error {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return errors.New("invalid IP address")
	}

	// Always forbidden categories
	if addr.IsUnspecified() {
		return errors.New("unspecified IP address destination is forbidden")
	}
	if addr.IsMulticast() {
		return errors.New("multicast IP address destination is forbidden")
	}
	if addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return errors.New("link-local IP address destination is forbidden")
	}
	if addr == cloudMetadataIPv6 || addr == cloudMetadataAli {
		return errors.New("cloud metadata destination is forbidden")
	}

	// If explicit CIDRs configured, address must match at least one
	if len(policy.AllowedCIDRs) > 0 {
		for _, prefix := range policy.AllowedCIDRs {
			if prefix.Contains(addr) {
				return nil
			}
		}
		return fmt.Errorf("destination IP %s does not match any allowed CIDR prefix", addr)
	}

	// No CIDRs configured: permit public unicast only
	if addr.IsLoopback() {
		return fmt.Errorf("loopback destination %s requires an explicit matching CIDR", addr)
	}
	if addr.IsPrivate() {
		return fmt.Errorf("private destination %s requires an explicit matching CIDR", addr)
	}
	if cgnatPrefix.Contains(addr) {
		return fmt.Errorf("carrier-grade NAT destination %s requires an explicit matching CIDR", addr)
	}
	for _, p := range specialNonPublicPrefixes {
		if p.Contains(addr) {
			return fmt.Errorf("destination %s is in reserved non-public range %s and requires an explicit matching CIDR", addr, p)
		}
	}
	if !addr.IsGlobalUnicast() {
		return fmt.Errorf("destination %s is not a permitted public unicast address", addr)
	}

	return nil
}

type pinnedRoundTripper struct {
	transport    *http.Transport
	expectedHost string
	expectedPort string
	expectedPath string
}

func (rt *pinnedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil {
		return nil, errors.New("request URL cannot be nil")
	}
	if req.URL.Scheme != "https" {
		return nil, errors.New("request scheme must be https")
	}
	if req.URL.User != nil {
		return nil, errors.New("request must not contain userinfo")
	}
	if req.URL.Hostname() != rt.expectedHost {
		return nil, fmt.Errorf("request host %q does not match pinned endpoint host %q", req.URL.Hostname(), rt.expectedHost)
	}
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	if port != rt.expectedPort {
		return nil, fmt.Errorf("request port %q does not match pinned endpoint port %q", port, rt.expectedPort)
	}
	if req.URL.Path != rt.expectedPath {
		return nil, fmt.Errorf("request path %q does not match pinned endpoint path %q", req.URL.Path, rt.expectedPath)
	}
	if req.URL.RawPath != "" && req.URL.RawPath != req.URL.Path {
		return nil, errors.New("request must not contain encoded path variants")
	}
	if req.URL.EscapedPath() != req.URL.Path {
		return nil, errors.New("request must not contain encoded path variants")
	}
	if req.URL.RawQuery != "" || req.URL.ForceQuery {
		return nil, errors.New("request must not contain query parameters")
	}
	if req.URL.Fragment != "" {
		return nil, errors.New("request must not contain fragment")
	}
	if req.Host != "" && req.Host != rt.expectedHost && req.Host != net.JoinHostPort(rt.expectedHost, rt.expectedPort) {
		return nil, errors.New("request Host header does not match pinned endpoint host")
	}

	return rt.transport.RoundTrip(req)
}

// NewPinnedHTTPClient creates a dedicated HTTP client for connecting to the specified
// probe endpoint. It enforces TLS 1.3, leaf certificate SHA-256 fingerprint verification,
// strict endpoint policy, 10-second handshake timeouts, and redirects rejection.
func NewPinnedHTTPClient(endpoint, fingerprint string, policy EndpointPolicy) (*http.Client, error) {
	// Clone policy.AllowedCIDRs to prevent caller mutation from broadening authorization
	clonedCIDRs := make([]netip.Prefix, len(policy.AllowedCIDRs))
	copy(clonedCIDRs, policy.AllowedCIDRs)
	policy = EndpointPolicy{AllowedCIDRs: clonedCIDRs}

	// Validate allowed CIDR prefixes
	for _, prefix := range policy.AllowedCIDRs {
		if !prefix.IsValid() {
			return nil, fmt.Errorf("invalid allowed CIDR prefix: %v", prefix)
		}
	}

	// Validate fingerprint: exactly 64 lowercase hexadecimal characters
	if len(fingerprint) != 64 {
		return nil, errors.New("fingerprint must be exactly 64 lowercase hexadecimal characters")
	}
	for i := 0; i < len(fingerprint); i++ {
		b := fingerprint[i]
		if !((b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')) {
			return nil, errors.New("fingerprint must be exactly 64 lowercase hexadecimal characters")
		}
	}
	expectedFingerprintBytes, err := hex.DecodeString(fingerprint)
	if err != nil {
		return nil, fmt.Errorf("decode fingerprint: %w", err)
	}

	// Parse and validate endpoint URL. Do not wrap url.Parse error to avoid leaking credentials.
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, errors.New("invalid endpoint URL: malformed syntax")
	}
	if !u.IsAbs() {
		return nil, errors.New("endpoint must be an absolute URL")
	}
	if u.Scheme != "wss" {
		return nil, errors.New("endpoint scheme must be wss")
	}
	if u.User != nil {
		return nil, errors.New("endpoint must not contain userinfo")
	}
	if u.RawQuery != "" || u.ForceQuery || strings.Contains(endpoint, "?") {
		return nil, errors.New("endpoint must not contain query parameters")
	}
	if u.Fragment != "" || u.RawFragment != "" || strings.Contains(endpoint, "#") {
		return nil, errors.New("endpoint must not contain fragment")
	}

	// Path validation: exactly /ws/probe/v1 or /ws/probe/enroll/v1
	if u.Path != "/ws/probe/v1" && u.Path != "/ws/probe/enroll/v1" {
		return nil, errors.New("endpoint path must be /ws/probe/v1 or /ws/probe/enroll/v1")
	}
	if u.RawPath != "" && u.RawPath != u.Path {
		return nil, errors.New("endpoint path must not contain encoded characters")
	}
	if u.EscapedPath() != u.Path {
		return nil, errors.New("endpoint path must not contain encoded characters")
	}

	expectedHost := u.Hostname()
	if expectedHost == "" {
		return nil, errors.New("endpoint host is required")
	}
	expectedPortStr := u.Port()
	if expectedPortStr == "" {
		expectedPortStr = "443"
	} else {
		portNum, err := strconv.ParseUint(expectedPortStr, 10, 16)
		if err != nil || portNum == 0 {
			return nil, errors.New("endpoint port must be a valid 16-bit integer (1-65535)")
		}
	}

	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		ServerName:         expectedHost,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("missing peer certificate")
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("parse peer certificate: %w", err)
			}
			now := time.Now().UTC()
			if now.Before(leaf.NotBefore) {
				return errors.New("peer certificate is not yet valid")
			}
			if !now.Before(leaf.NotAfter) {
				return errors.New("peer certificate has expired")
			}
			leafHash := sha256.Sum256(rawCerts[0])
			if subtle.ConstantTimeCompare(leafHash[:], expectedFingerprintBytes) != 1 {
				return domain.ErrProbeCertificateMismatch
			}
			return nil
		},
	}

	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		Proxy:                 nil, // Disable environment proxies
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			dialHost, dialPort, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("invalid dial address %q: %w", addr, err)
			}
			if dialHost != expectedHost || dialPort != expectedPortStr {
				return nil, fmt.Errorf("dial address %s:%s does not match pinned endpoint %s:%s", dialHost, dialPort, expectedHost, expectedPortStr)
			}

			var ips []netip.Addr
			if parsedAddr, err := netip.ParseAddr(expectedHost); err == nil {
				ips = []netip.Addr{parsedAddr}
			} else {
				resolved, err := net.DefaultResolver.LookupNetIP(dialCtx, "ip", expectedHost)
				if err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					return nil, fmt.Errorf("resolve endpoint host: %w", err)
				}
				ips = resolved
			}

			if len(ips) == 0 {
				return nil, errors.New("no IP addresses resolved for endpoint host")
			}

			var permittedIPs []netip.Addr
			var lastPolicyErr error
			for _, ip := range ips {
				if err := ValidateDestination(ip, policy); err != nil {
					lastPolicyErr = err
					continue
				}
				permittedIPs = append(permittedIPs, ip)
			}

			if len(permittedIPs) == 0 {
				if lastPolicyErr != nil {
					return nil, fmt.Errorf("destination rejected by policy: %w", lastPolicyErr)
				}
				return nil, errors.New("all destination IPs rejected by policy")
			}

			var lastDialErr error
			for _, ip := range permittedIPs {
				target := net.JoinHostPort(ip.String(), expectedPortStr)
				conn, err := dialer.DialContext(dialCtx, "tcp", target)
				if err == nil {
					return conn, nil
				}
				lastDialErr = err
			}

			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("connect to endpoint: %w", lastDialErr)
		},
	}

	client := &http.Client{
		Transport: &pinnedRoundTripper{
			transport:    transport,
			expectedHost: expectedHost,
			expectedPort: expectedPortStr,
			expectedPath: u.Path,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errors.New("redirects are not allowed")
		},
	}

	return client, nil
}
