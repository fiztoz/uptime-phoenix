package checker

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func startCheckerDNSServer(t *testing.T) string {
	t.Helper()
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for local DNS server: %v", err)
	}
	mux := dns.NewServeMux()
	mux.HandleFunc(".", func(w dns.ResponseWriter, req *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(req)
		if len(req.Question) == 0 {
			response.Rcode = dns.RcodeFormatError
			_ = w.WriteMsg(response)
			return
		}
		question := req.Question[0]
		if question.Name == "missing.invalid." {
			response.Rcode = dns.RcodeNameError
			_ = w.WriteMsg(response)
			return
		}
		var record dns.RR
		switch question.Qtype {
		case dns.TypeA:
			record = &dns.A{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("192.0.2.10")}
		case dns.TypeMX:
			record = &dns.MX{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeMX, Class: dns.ClassINET, Ttl: 60}, Preference: 10, Mx: "mail.example.test."}
		case dns.TypeTXT:
			record = &dns.TXT{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60}, Txt: []string{"phoenix-checker"}}
		default:
			response.Rcode = dns.RcodeNameError
			_ = w.WriteMsg(response)
			return
		}
		response.Answer = []dns.RR{record}
		_ = w.WriteMsg(response)
	})
	server := &dns.Server{PacketConn: packetConn, Handler: mux}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() {
		if err := server.Shutdown(); err != nil {
			t.Errorf("stop local DNS server: %v", err)
		}
	})
	return packetConn.LocalAddr().String()
}

func localDNSChecker(t *testing.T, resolver, questionName string, questionType uint16) DNSChecker {
	t.Helper()
	localAddress := startCheckerDNSServer(t)
	return DNSChecker{exchangeFunc: func(ctx context.Context, client *dns.Client, msg *dns.Msg, configuredAddress string) (*dns.Msg, time.Duration, error) {
		if resolver != "" && configuredAddress != resolver {
			t.Errorf("DNS resolver address = %q, want %q", configuredAddress, resolver)
		}
		if len(msg.Question) != 1 || msg.Question[0].Name != questionName || msg.Question[0].Qtype != questionType {
			t.Errorf("DNS question = %+v, want %s type %d", msg.Question, questionName, questionType)
		}
		return client.ExchangeContext(ctx, msg, localAddress)
	}}
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
		{name: "missing hostname", config: map[string]any{}, wantErr: true},
		{name: "empty hostname", config: map[string]any{"hostname": ""}, wantErr: true},
		{name: "nil hostname", config: map[string]any{"hostname": nil}, wantErr: true},
		{name: "valid config", config: map[string]any{"hostname": "example.test"}},
		{name: "valid with resolve_type A", config: map[string]any{"hostname": "example.test", "resolve_type": "A"}},
		{name: "valid with resolve_type MX", config: map[string]any{"hostname": "example.test", "resolve_type": "MX"}},
		{name: "invalid resolve_type", config: map[string]any{"hostname": "example.test", "resolve_type": "INVALID"}, wantErr: true},
		{name: "all optional fields", config: map[string]any{"hostname": "example.test", "resolve_type": "TXT", "resolve_server": "127.0.0.1", "expected_value": "phoenix", "timeout": 5.0}},
		{name: "negative timeout", config: map[string]any{"hostname": "example.test", "timeout": float64(-1)}, wantErr: true},
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
	c := localDNSChecker(t, "127.0.0.1:53", "example.test.", dns.TypeA)
	result, err := c.Check(context.Background(), map[string]any{"hostname": "example.test", "resolve_type": "A", "resolve_server": "127.0.0.1", "timeout": 2.0})
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" || !strings.Contains(result.Message, "192.0.2.10") {
		t.Fatalf("Check() = %+v, want UP with local A answer", result)
	}
	if result.LatencyMs < 0 {
		t.Errorf("Check() latency = %d, want >= 0", result.LatencyMs)
	}
}

func TestDNSChecker_Check_MX(t *testing.T) {
	c := localDNSChecker(t, "127.0.0.1:53", "example.test.", dns.TypeMX)
	result, err := c.Check(context.Background(), map[string]any{"hostname": "example.test", "resolve_type": "MX", "resolve_server": "127.0.0.1", "timeout": 2.0})
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" || !strings.Contains(result.Message, "mail.example.test") {
		t.Fatalf("Check() = %+v, want UP with local MX answer", result)
	}
}

func TestDNSChecker_Check_TXT(t *testing.T) {
	c := localDNSChecker(t, "127.0.0.1:53", "example.test.", dns.TypeTXT)
	result, err := c.Check(context.Background(), map[string]any{"hostname": "example.test", "resolve_type": "TXT", "resolve_server": "127.0.0.1", "timeout": 2.0})
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" || !strings.Contains(result.Message, "phoenix-checker") {
		t.Fatalf("Check() = %+v, want UP with local TXT answer", result)
	}
}

func TestDNSChecker_Check_ExpectedValue_Pass(t *testing.T) {
	c := localDNSChecker(t, "127.0.0.1:53", "example.test.", dns.TypeA)
	result, err := c.Check(context.Background(), map[string]any{"hostname": "example.test", "resolve_type": "A", "resolve_server": "127.0.0.1", "expected_value": "192.0.2.10", "timeout": 2.0})
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Fatalf("Check() = %+v, want UP", result)
	}
}

func TestDNSChecker_Check_ExpectedValue_Fail(t *testing.T) {
	c := localDNSChecker(t, "127.0.0.1:53", "example.test.", dns.TypeA)
	result, err := c.Check(context.Background(), map[string]any{"hostname": "example.test", "resolve_type": "A", "resolve_server": "127.0.0.1", "expected_value": "not-present", "timeout": 2.0})
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" || !strings.Contains(result.Message, "not found") {
		t.Fatalf("Check() = %+v, want expected-value failure", result)
	}
}

func TestDNSChecker_Check_NonexistentDomain(t *testing.T) {
	c := localDNSChecker(t, "127.0.0.1:53", "missing.invalid.", dns.TypeA)
	result, err := c.Check(context.Background(), map[string]any{"hostname": "missing.invalid", "resolve_type": "A", "resolve_server": "127.0.0.1", "timeout": 2.0})
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" || !strings.Contains(result.Message, "NXDOMAIN") {
		t.Fatalf("Check() = %+v, want NXDOMAIN DOWN", result)
	}
}

func TestDNSChecker_Check_DefaultServer(t *testing.T) {
	c := localDNSChecker(t, "8.8.8.8:53", "example.test.", dns.TypeA)
	result, err := c.Check(context.Background(), map[string]any{"hostname": "example.test", "timeout": 2.0})
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" || !strings.Contains(result.Message, "192.0.2.10") {
		t.Fatalf("default resolver check = %+v, want local fixture A answer", result)
	}
}

func TestDNSChecker_Check_ExchangeError(t *testing.T) {
	c := DNSChecker{exchangeFunc: func(context.Context, *dns.Client, *dns.Msg, string) (*dns.Msg, time.Duration, error) {
		return nil, 0, net.ErrClosed
	}}
	result, err := c.Check(context.Background(), map[string]any{"hostname": "example.test", "resolve_server": "127.0.0.1", "timeout": 1.0})
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" || !strings.Contains(result.Message, "DNS query failed") {
		t.Fatalf("Check() = %+v, want transport failure DOWN", result)
	}
}

func TestDNSChecker_Check_BadServer(t *testing.T) {
	c := DNSChecker{}
	result, err := c.Check(context.Background(), map[string]any{"hostname": "example.test", "resolve_type": "A", "resolve_server": "10.255.255.1", "timeout": 0.05})
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Fatalf("Check() = %+v, want DOWN", result)
	}
}
