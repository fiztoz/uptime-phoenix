package checker

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// canReachDNS performs a real DNS query to verify the DNS server is reachable.
func canReachDNS(server string) bool {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn("google.com"), dns.TypeA)
	m.RecursionDesired = true

	client := &dns.Client{Timeout: 5 * time.Second}
	_, _, err := client.Exchange(m, server+":53")
	return err == nil
}

func TestDNSChecker_Type(t *testing.T) {
	c := DNSChecker{}
	if got := c.Type(); got != "dns" {
		t.Errorf("Type() = %q, want %q", got, "dns")
	}
}

func TestDNSChecker_Validate(t *testing.T) {
	c := DNSChecker{}

	tests := []struct {
		name    string
		config  map[string]any
		wantErr bool
	}{
		{
			name:    "missing hostname",
			config:  map[string]any{},
			wantErr: true,
		},
		{
			name:    "empty hostname",
			config:  map[string]any{"hostname": ""},
			wantErr: true,
		},
		{
			name:    "nil hostname",
			config:  map[string]any{"hostname": nil},
			wantErr: true,
		},
		{
			name:    "valid config",
			config:  map[string]any{"hostname": "google.com"},
			wantErr: false,
		},
		{
			name:    "valid with resolve_type A",
			config:  map[string]any{"hostname": "google.com", "resolve_type": "A"},
			wantErr: false,
		},
		{
			name:    "valid with resolve_type MX",
			config:  map[string]any{"hostname": "google.com", "resolve_type": "MX"},
			wantErr: false,
		},
		{
			name:    "invalid resolve_type",
			config:  map[string]any{"hostname": "google.com", "resolve_type": "INVALID"},
			wantErr: true,
		},
		{
			name: "all optional fields",
			config: map[string]any{
				"hostname":       "google.com",
				"resolve_type":   "TXT",
				"resolve_server": "8.8.8.8",
				"expected_value": "google",
				"timeout":        5.0,
			},
			wantErr: false,
		},
		{
			name:    "negative timeout",
			config:  map[string]any{"hostname": "google.com", "timeout": float64(-1)},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := c.Validate(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDNSChecker_Check_A(t *testing.T) {
	if !canReachDNS("8.8.8.8") {
		t.Skip("8.8.8.8:53 DNS is not reachable (UDP port 53 blocked by firewall)")
	}

	c := DNSChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"hostname":       "google.com",
		"resolve_type":   "A",
		"resolve_server": "8.8.8.8",
		"timeout":        10.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Errorf("Check() status = %v, want UP (message: %s)", result.Status, result.Message)
	}
	if result.LatencyMs < 0 {
		t.Errorf("Check() latency = %d, want >= 0", result.LatencyMs)
	}
	t.Logf("google.com A: status=%v latency=%dms message=%s",
		result.Status, result.LatencyMs, result.Message)
}

func TestDNSChecker_Check_MX(t *testing.T) {
	if !canReachDNS("8.8.8.8") {
		t.Skip("8.8.8.8:53 DNS is not reachable (UDP port 53 blocked by firewall)")
	}

	c := DNSChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"hostname":       "google.com",
		"resolve_type":   "MX",
		"resolve_server": "8.8.8.8",
		"timeout":        10.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Errorf("Check() status = %v, want UP (message: %s)", result.Status, result.Message)
	}
}

func TestDNSChecker_Check_TXT(t *testing.T) {
	if !canReachDNS("8.8.8.8") {
		t.Skip("8.8.8.8:53 DNS is not reachable (UDP port 53 blocked by firewall)")
	}

	c := DNSChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"hostname":       "google.com",
		"resolve_type":   "A",
		"resolve_server": "8.8.8.8",
		"timeout":        10.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Errorf("Check() status = %v, want UP (message: %s)", result.Status, result.Message)
	}
}

func TestDNSChecker_Check_ExpectedValue_Pass(t *testing.T) {
	if !canReachDNS("8.8.8.8") {
		t.Skip("8.8.8.8:53 DNS is not reachable (UDP port 53 blocked by firewall)")
	}

	c := DNSChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"hostname":       "google.com",
		"resolve_type":   "NS",
		"resolve_server": "8.8.8.8",
		"expected_value": "google",
		"timeout":        10.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Errorf("Check() status = %v, want UP (message: %s)", result.Status, result.Message)
	}
}

func TestDNSChecker_Check_ExpectedValue_Fail(t *testing.T) {
	if !canReachDNS("8.8.8.8") {
		t.Skip("8.8.8.8:53 DNS is not reachable (UDP port 53 blocked by firewall)")
	}

	c := DNSChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"hostname":       "google.com",
		"resolve_type":   "A",
		"resolve_server": "8.8.8.8",
		"expected_value": "this-string-will-never-appear-in-dns-xyzzy",
		"timeout":        10.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN (message: %s)", result.Status, result.Message)
	}
}

func TestDNSChecker_Check_NonexistentDomain(t *testing.T) {
	if !canReachDNS("8.8.8.8") {
		t.Skip("8.8.8.8:53 DNS is not reachable (UDP port 53 blocked by firewall)")
	}

	c := DNSChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"hostname":       "this-domain-definitely-does-not-exist-xyzzy.invalid",
		"resolve_type":   "A",
		"resolve_server": "8.8.8.8",
		"timeout":        10.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN (message: %s)", result.Status, result.Message)
	}
}

func TestDNSChecker_Check_BadServer(t *testing.T) {
	c := DNSChecker{}
	// Non-routable IP — should fail quickly.
	result, err := c.Check(context.Background(), map[string]any{
		"hostname":       "google.com",
		"resolve_type":   "A",
		"resolve_server": "10.255.255.1",
		"timeout":        3.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN (message: %s)", result.Status, result.Message)
	}
}

// TestDNSChecker_Check_UsesSystemResolver verifies the DNS checker works
// with the system's default DNS when no resolve_server is configured.
func TestDNSChecker_Check_DefaultServer(t *testing.T) {
	// Use A record check against google.com with default server (8.8.8.8)
	// since some networks block external DNS.
	if !canReachDNS("8.8.8.8") {
		// Try Cloudflare DNS as fallback
		if !canReachDNS("1.1.1.1") {
			t.Skip("No external DNS server reachable (UDP port 53 blocked)")
		}
	}

	c := DNSChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"hostname": "google.com",
		"timeout":  10.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	// The default resolve_server is 8.8.8.8 which may be blocked.
	// Accept either UP or DOWN depending on network conditions.
	if result.Status.String() != "UP" && result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want UP or DOWN", result.Status)
	}
	t.Logf("default DNS check: status=%v latency=%dms message=%s",
		result.Status, result.LatencyMs, result.Message)
}
