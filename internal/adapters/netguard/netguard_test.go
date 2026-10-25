package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestPolicy_BlocksInternalRanges(t *testing.T) {
	p := NewPolicy(nil)
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "0.0.0.0", "10.0.0.1", "172.16.0.1", "172.31.255.254",
		"192.168.1.1", "169.254.169.254", "169.254.0.1", "100.64.0.1", "100.127.255.254",
		"224.0.0.1", "255.255.255.255", "192.0.0.1",
		"::1", "::", "fc00::1", "fd00:ec2::254", "fe80::1", "ff02::1",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "::ffff:10.0.0.1",
		"64:ff9b::a9fe:a9fe", // NAT64 form of 169.254.169.254
		"64:ff9b:1::1",
	}
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

func TestPolicy_AllowlistOverridesBlock(t *testing.T) {
	prefixes, err := ParseAllowlist(" 10.20.0.0/16, 127.0.0.1 ,fd12::/16,")
	if err != nil {
		t.Fatalf("ParseAllowlist: %v", err)
	}
	p := NewPolicy(prefixes)
	for _, s := range []string{"10.20.3.4", "127.0.0.1", "::ffff:127.0.0.1", "fd12::5"} {
		if !p.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s blocked; want allowlisted", s)
		}
	}
	for _, s := range []string{"10.21.0.1", "127.0.0.2", "169.254.169.254", "fd13::1"} {
		if p.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed; only the allowlisted ranges should be", s)
		}
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
}

// Every redirect hop dials through the guard.
func TestPolicy_ClientRefusesRedirectToMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	p := NewPolicy([]netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	resp, err := p.Client(5 * time.Second).Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("redirect to the metadata address succeeded; want blocked")
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v; want ErrBlocked", err)
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
