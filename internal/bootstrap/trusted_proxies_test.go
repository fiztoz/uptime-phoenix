package bootstrap

import (
	"reflect"
	"strings"
	"testing"
)

func TestTrustedProxyCIDRs(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{"10.42.0.8/32", []string{"10.42.0.8/32"}},
		{" 10.42.0.8/24, fd00:1234::8/64 ", []string{"10.42.0.0/24", "fd00:1234::/64"}},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			prefixes, err := parseTrustedProxyCIDRs(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, prefix := range prefixes {
				got = append(got, prefix.String())
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("prefixes=%v; want %v", got, tc.want)
			}
		})
	}
}

func TestTrustedProxyCIDRsRejectUnsafeInput(t *testing.T) {
	for _, raw := range []string{"0.0.0.0/0", "::/0", "::ffff:10.42.0.8/128", "127.0.0.1", "proxy.example.com/32", "10.42.0.8/32,", ",", "not-a-prefix"} {
		t.Run(raw, func(t *testing.T) {
			prefixes, err := parseTrustedProxyCIDRs(raw)
			if err == nil || prefixes != nil || !strings.Contains(err.Error(), "TRUSTED_PROXY_CIDRS") {
				t.Fatalf("unsafe proxy allow-list accepted: prefixes=%v err=%v", prefixes, err)
			}
			if strings.Contains(err.Error(), raw) {
				t.Fatal("arbitrary environment contents leaked into error")
			}
		})
	}
}

func TestTrustedProxyConfigLoadAndRunFailClosed(t *testing.T) {
	t.Setenv("JWT_SECRET", "trusted-proxy-config-test-secret-with-enough-length")
	t.Setenv("TRUSTED_PROXY_CIDRS", "invalid-proxy-prefix")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXY_CIDRS") {
		t.Fatalf("environment configuration did not fail closed: %v", err)
	}
	// Programmatic callers must fail before opening a DB or starting telemetry.
	if err := Run(Config{TrustedProxyCIDRs: "invalid-proxy-prefix"}); err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXY_CIDRS") {
		t.Fatalf("programmatic startup did not fail closed: %v", err)
	}
}
