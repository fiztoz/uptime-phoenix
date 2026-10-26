package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

var floorAddrs = []string{
	"0.0.0.0", "::", "169.254.169.254", "169.254.0.1", "fe80::1",
	"224.0.0.1", "ff02::1", "fd00:ec2::254", "100.100.100.200", "168.63.129.16",
	"::ffff:169.254.169.254", "::ffff:0.0.0.0",
	"64:ff9b::a9fe:a9fe", // NAT64 form of 169.254.169.254
}

func TestRestrictedPolicy_BlocksInternalRanges(t *testing.T) {
	p := NewPolicy(nil)
	blocked := append([]string{
		"127.0.0.1", "127.1.2.3", "10.0.0.1", "172.16.0.1", "172.31.255.254",
		"192.168.1.1", "100.64.0.1", "100.127.255.254",
		"255.255.255.255", "192.0.0.1", "0.1.2.3",
		"::1", "fc00::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1",
		"64:ff9b::a00:1", // NAT64 form of 10.0.0.1
		"64:ff9b:1::1",
	}, floorAddrs...)
	for _, s := range blocked {
		if p.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed; want blocked", s)
		}
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "172.32.0.1", "100.128.0.1",
		"198.18.0.1", // fake-IP DNS proxies hand out this range for public names
		"2606:4700:4700::1111", "64:ff9b::808:808"}
	for _, s := range allowed {
		if !p.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s blocked; want allowed", s)
		}
	}
}

// The admin tier reaches private ranges but never the floor.
func TestTrustedPolicy_AppliesOnlyTheFloor(t *testing.T) {
	p := NewTrustedPolicy()
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "100.64.0.1", "fc00::1", "::1", "::ffff:10.1.2.3"} {
		if !p.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s blocked for the admin tier; want allowed", s)
		}
	}
	for _, s := range floorAddrs {
		if p.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed for the admin tier; the floor must block it", s)
		}
	}
}

func TestPolicy_AllowlistOverridesBlockButNotFloor(t *testing.T) {
	prefixes, err := ParseAllowlist(" 10.20.0.0/16, 127.0.0.1 ,fd12::/16, ::ffff:192.168.7.0/120,")
	if err != nil {
		t.Fatalf("ParseAllowlist: %v", err)
	}
	p := NewPolicy(prefixes)
	for _, s := range []string{"10.20.3.4", "127.0.0.1", "::ffff:127.0.0.1", "fd12::5", "192.168.7.9", "::ffff:192.168.7.9"} {
		if !p.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s blocked; want allowlisted", s)
		}
	}
	for _, s := range []string{"10.21.0.1", "127.0.0.2", "169.254.169.254", "fd13::1", "192.168.8.1"} {
		if p.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed; only the allowlisted ranges should be", s)
		}
	}

	everything, err := ParseAllowlist("0.0.0.0/0,::/0")
	if err != nil {
		t.Fatal(err)
	}
	wide := NewPolicy(everything)
	for _, s := range floorAddrs {
		if wide.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed by a 0.0.0.0/0 allowlist; the floor must block it", s)
		}
	}
	if !wide.Allowed(netip.MustParseAddr("10.0.0.1")) {
		t.Error("10.0.0.1 blocked despite a 0.0.0.0/0 allowlist")
	}
}

// An IPv4-mapped allowlist entry describes IPv4 addresses, which the guard
// compares in unmapped form, so it is converted (or rejected when it is wider
// than the mapped space).
func TestParseAllowlist_IPv4MappedEntries(t *testing.T) {
	got, err := ParseAllowlist("::ffff:10.0.0.0/104, ::ffff:192.168.1.5")
	if err != nil {
		t.Fatalf("ParseAllowlist: %v", err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.168.1.5/32")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ParseAllowlist = %v; want %v", got, want)
	}
	if !NewPolicy(got).Allowed(netip.MustParseAddr("10.9.8.7")) {
		t.Fatal("::ffff:10.0.0.0/104 does not allow 10.9.8.7")
	}
	if _, err := ParseAllowlist("::ffff:0.0.0.0/90"); err == nil {
		t.Fatal("an IPv4-mapped prefix shorter than /96 was accepted")
	}
}

func TestParseAllowlist_RejectsGarbage(t *testing.T) {
	for _, raw := range []string{"10.0.0.0/33", "not-an-ip", "10.0.0.0/8,example.com"} {
		if _, err := ParseAllowlist(raw); err == nil {
			t.Errorf("ParseAllowlist(%q) succeeded; want error", raw)
		}
	}
	if got, err := ParseAllowlist(""); err != nil || len(got) != 0 {
		t.Errorf("ParseAllowlist(\"\") = %v, %v; want empty, nil", got, err)
	}
}

func TestForContext_PicksTierFromOwnerTrust(t *testing.T) {
	bg := context.Background()
	if ForContext(bg).Trusted() {
		t.Fatal("no trust attached: got the admin tier; want restricted (fail closed)")
	}
	if ForContext(domain.WithOutboundTrust(bg, domain.OutboundTrustRestricted)).Trusted() {
		t.Fatal("restricted trust: got the admin tier")
	}
	if !ForContext(domain.WithOutboundTrust(bg, domain.OutboundTrustAdmin)).Trusted() {
		t.Fatal("admin trust: got the restricted tier")
	}
	if !ForContext(domain.WithOutboundTrust(bg, domain.OutboundTrustProbe)).Trusted() {
		t.Fatal("probe trust: got the restricted tier")
	}
}

// The guard runs on the resolved address, so a hostname that resolves to
// loopback is refused the same as a literal IP.
func TestPolicy_DialRefusesResolvedInternalAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, host := range []string{"127.0.0.1", "localhost"} {
		_, err := NewPolicy(nil).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("dial %s: err = %v; want ErrBlocked", host, err)
		}
	}
	conn, err := NewPolicy([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}).
		DialContext(ctx, "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("allowlisted dial: %v", err)
	}
	_ = conn.Close()
	conn, err = NewTrustedPolicy().DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("admin-tier dial: %v", err)
	}
	_ = conn.Close()
}

// Every redirect hop dials through the guard, for both tiers.
func TestPolicy_ClientRefusesRedirectToMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	for name, p := range map[string]*Policy{
		"restricted": NewPolicy([]netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}),
		"admin":      NewTrustedPolicy(),
	} {
		resp, err := p.Client(5 * time.Second).Get(srv.URL)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("%s: redirect to the metadata address succeeded; want blocked", name)
		}
		if !errors.Is(err, ErrBlocked) {
			t.Fatalf("%s: err = %v; want ErrBlocked", name, err)
		}
	}
}

// Transport() wraps http.ProxyFromEnvironment with the proxy guard. The
// stdlib caches the proxy environment once per process, so this drives the
// same guard with an environment-style proxy function: a restricted owner
// cannot use the proxy to reach a blocked target (the proxy is never
// contacted), while an admin owner can, and the proxy itself (on loopback,
// as an in-cluster egress proxy would be on a private address) is reachable
// by both tiers.
func TestTransport_EnvironmentStyleProxyCannotReachBlockedTarget(t *testing.T) {
	var proxyHits atomic.Int32
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
	}))
	defer proxySrv.Close()
	proxyURL, _ := url.Parse(proxySrv.URL)
	envProxy := http.ProxyURL(proxyURL)

	get := func(p *Policy, target string) error {
		client := &http.Client{Transport: p.transportWithProxy(envProxy), Timeout: 5 * time.Second}
		resp, err := client.Get(target)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}

	if err := get(NewPolicy(nil), "http://10.255.255.1/"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("restricted owner via env proxy: err = %v; want ErrBlocked", err)
	}
	if err := get(NewPolicy(nil), "http://internal.invalid/"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("restricted owner via env proxy, unresolvable name: err = %v; want ErrBlocked", err)
	}
	if n := proxyHits.Load(); n != 0 {
		t.Fatalf("proxy contacted %d times for blocked targets; want 0", n)
	}
	if err := get(NewPolicy(nil), "http://93.184.216.34/"); err != nil {
		t.Fatalf("restricted owner via env proxy to a public address: %v", err)
	}
	if err := get(NewTrustedPolicy(), "http://10.255.255.1/"); err != nil {
		t.Fatalf("admin owner via env proxy: %v", err)
	}
	if err := get(NewTrustedPolicy(), "http://169.254.169.254/"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("admin owner via env proxy to metadata: err = %v; want ErrBlocked", err)
	}
	if n := proxyHits.Load(); n != 2 {
		t.Fatalf("proxy contacted %d times; want 2", n)
	}
}

func TestValidate_RejectsBadEnv(t *testing.T) {
	t.Setenv(EnvAllowedCIDRs, "10.0.0.0/8,bogus")
	if err := Validate(); err == nil {
		t.Fatal("Validate accepted an invalid OUTBOUND_ALLOWED_CIDRS")
	}
	t.Setenv(EnvAllowedCIDRs, "10.0.0.0/8, fd00::/8")
	if err := Validate(); err != nil {
		t.Fatalf("Validate rejected a valid value: %v", err)
	}
}
