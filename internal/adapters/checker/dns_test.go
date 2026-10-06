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

// --- Authority-section / NODATA evidence (issue #53) -------------------------
//
// A NOERROR response with an empty Answer section is a NODATA answer: the
// authority SOA describes negative caching and is never evidence that the
// requested record exists.

func soaRecord(name string) dns.RR {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: name, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60},
		Ns:      "ns1.example.test.",
		Mbox:    "hostmaster.example.test.",
		Serial:  1,
		Refresh: 3600,
		Retry:   600,
		Expire:  86400,
		Minttl:  60,
	}
}

func nsRecord(name, target string) dns.RR {
	return &dns.NS{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 60}, Ns: target}
}

// localDNSFixtureChecker runs a local DNS server whose response for the single
// expected question is shaped by fixture (after a standard NOERROR reply),
// letting tests script authority-only (NODATA, delegation) and CNAME answer
// shapes through the real checker.
func localDNSFixtureChecker(t *testing.T, questionName string, questionType uint16, fixture func(*dns.Msg)) DNSChecker {
	t.Helper()
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for local DNS server: %v", err)
	}
	mux := dns.NewServeMux()
	mux.HandleFunc(".", func(w dns.ResponseWriter, req *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(req)
		if len(req.Question) == 1 {
			fixture(response)
		}
		_ = w.WriteMsg(response)
	})
	server := &dns.Server{PacketConn: packetConn, Handler: mux}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() {
		if err := server.Shutdown(); err != nil {
			t.Errorf("stop local DNS server: %v", err)
		}
	})
	localAddress := packetConn.LocalAddr().String()
	return DNSChecker{exchangeFunc: func(ctx context.Context, client *dns.Client, msg *dns.Msg, configuredAddress string) (*dns.Msg, time.Duration, error) {
		if configuredAddress != "127.0.0.1:53" {
			t.Errorf("DNS resolver address = %q, want %q", configuredAddress, "127.0.0.1:53")
		}
		if len(msg.Question) != 1 || msg.Question[0].Name != questionName || msg.Question[0].Qtype != questionType {
			t.Errorf("DNS question = %+v, want %s type %d", msg.Question, questionName, questionType)
		}
		return client.ExchangeContext(ctx, msg, localAddress)
	}}
}

func TestDNSChecker_Check_NODATA_AuthoritySOA_Down(t *testing.T) {
	for _, tt := range []struct {
		resolveType string
		dnsType     uint16
	}{
		{"A", dns.TypeA},
		{"AAAA", dns.TypeAAAA},
		{"MX", dns.TypeMX},
	} {
		t.Run(tt.resolveType, func(t *testing.T) {
			// Standard authoritative NODATA: NOERROR, empty Answer, SOA for the
			// queried name in the authority section.
			c := localDNSFixtureChecker(t, "empty.test.", tt.dnsType, func(resp *dns.Msg) {
				resp.Ns = []dns.RR{soaRecord("empty.test.")}
			})
			result, err := c.Check(context.Background(), map[string]any{"hostname": "empty.test", "resolve_type": tt.resolveType, "resolve_server": "127.0.0.1", "timeout": 2.0})
			if err != nil {
				t.Fatalf("Check() returned unexpected error: %v", err)
			}
			want := "no " + tt.resolveType + " records found"
			if result.Status.String() != "DOWN" || !strings.Contains(result.Message, want) {
				t.Fatalf("Check() = %+v, want DOWN %q (authority SOA is not evidence of the requested record)", result, want)
			}
		})
	}
}

func TestDNSChecker_Check_SOA_AuthoritySection_Up(t *testing.T) {
	// Intentional SOA/NS behavior: an SOA of the queried name delivered in the
	// authority section (zone apex) is valid evidence for a SOA check.
	c := localDNSFixtureChecker(t, "empty.test.", dns.TypeSOA, func(resp *dns.Msg) {
		resp.Ns = []dns.RR{soaRecord("empty.test.")}
	})
	result, err := c.Check(context.Background(), map[string]any{"hostname": "empty.test", "resolve_type": "SOA", "resolve_server": "127.0.0.1", "timeout": 2.0})
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" || !strings.Contains(result.Message, "(serial 1)") {
		t.Fatalf("Check() = %+v, want UP with authority-section SOA", result)
	}
}

func TestDNSChecker_Check_NS_DelegationAuthority_Up(t *testing.T) {
	// Intentional SOA/NS behavior: a delegation's NS records for the queried
	// name arrive in the authority section and are valid NS evidence.
	c := localDNSFixtureChecker(t, "delegated.test.", dns.TypeNS, func(resp *dns.Msg) {
		resp.Ns = []dns.RR{nsRecord("delegated.test.", "ns1.example.test.")}
	})
	result, err := c.Check(context.Background(), map[string]any{"hostname": "delegated.test", "resolve_type": "NS", "resolve_server": "127.0.0.1", "timeout": 2.0})
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" || !strings.Contains(result.Message, "ns1.example.test") {
		t.Fatalf("Check() = %+v, want UP with delegation NS records", result)
	}
}

func TestDNSChecker_Check_SOA_ParentAuthoritySOA_Down(t *testing.T) {
	// NODATA for a non-apex name: the authority SOA belongs to the parent zone,
	// not to the queried name, so it is not evidence of a SOA record here.
	c := localDNSFixtureChecker(t, "www.empty.test.", dns.TypeSOA, func(resp *dns.Msg) {
		resp.Ns = []dns.RR{soaRecord("empty.test.")}
	})
	result, err := c.Check(context.Background(), map[string]any{"hostname": "www.empty.test", "resolve_type": "SOA", "resolve_server": "127.0.0.1", "timeout": 2.0})
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" || !strings.Contains(result.Message, "no SOA records found") {
		t.Fatalf("Check() = %+v, want DOWN (parent-zone SOA is not evidence)", result)
	}
}

func TestDNSChecker_Check_AnswerWrongType_Down(t *testing.T) {
	// Evidence must be of the requested type: a TXT answer does not prove an A
	// record exists.
	c := localDNSFixtureChecker(t, "mixed.test.", dns.TypeA, func(resp *dns.Msg) {
		resp.Answer = []dns.RR{
			&dns.TXT{Hdr: dns.RR_Header{Name: "mixed.test.", Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60}, Txt: []string{"not-an-address"}},
		}
	})
	result, err := c.Check(context.Background(), map[string]any{"hostname": "mixed.test", "resolve_type": "A", "resolve_server": "127.0.0.1", "timeout": 2.0})
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" || !strings.Contains(result.Message, "no A records found") {
		t.Fatalf("Check() = %+v, want DOWN (answer of the wrong type is not evidence)", result)
	}
}

func TestDNSChecker_Check_CNAME_Resolution_Up(t *testing.T) {
	tests := []struct {
		name        string
		resolveType string
		dnsType     uint16
		fixture     func(*dns.Msg)
		wantMessage string
	}{
		{
			name:        "A query answered by CNAME chain",
			resolveType: "A",
			dnsType:     dns.TypeA,
			fixture: func(resp *dns.Msg) {
				resp.Answer = []dns.RR{
					&dns.CNAME{Hdr: dns.RR_Header{Name: "alias.test.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 60}, Target: "target.test."},
					&dns.A{Hdr: dns.RR_Header{Name: "target.test.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("192.0.2.10")},
				}
			},
			wantMessage: "192.0.2.10",
		},
		{
			name:        "A query answered by bare CNAME",
			resolveType: "A",
			dnsType:     dns.TypeA,
			fixture: func(resp *dns.Msg) {
				resp.Answer = []dns.RR{
					&dns.CNAME{Hdr: dns.RR_Header{Name: "alias.test.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 60}, Target: "target.test."},
				}
			},
			wantMessage: "target.test",
		},
		{
			name:        "CNAME query",
			resolveType: "CNAME",
			dnsType:     dns.TypeCNAME,
			fixture: func(resp *dns.Msg) {
				resp.Answer = []dns.RR{
					&dns.CNAME{Hdr: dns.RR_Header{Name: "alias.test.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 60}, Target: "target.test."},
				}
			},
			wantMessage: "target.test",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := localDNSFixtureChecker(t, "alias.test.", tt.dnsType, tt.fixture)
			result, err := c.Check(context.Background(), map[string]any{"hostname": "alias.test", "resolve_type": tt.resolveType, "resolve_server": "127.0.0.1", "timeout": 2.0})
			if err != nil {
				t.Fatalf("Check() returned unexpected error: %v", err)
			}
			if result.Status.String() != "UP" || !strings.Contains(result.Message, tt.wantMessage) {
				t.Fatalf("Check() = %+v, want UP mentioning %q (valid CNAME resolution must stay UP)", result, tt.wantMessage)
			}
		})
	}
}

func TestDNSChecker_Check_ExpectedValue_CNAMETarget_Pass(t *testing.T) {
	// expected_value matching must keep seeing CNAME targets inside a chain
	// answer (it matched every answer record before the NODATA fix).
	c := localDNSFixtureChecker(t, "alias.test.", dns.TypeA, func(resp *dns.Msg) {
		resp.Answer = []dns.RR{
			&dns.CNAME{Hdr: dns.RR_Header{Name: "alias.test.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 60}, Target: "target.test."},
			&dns.A{Hdr: dns.RR_Header{Name: "target.test.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("192.0.2.10")},
		}
	})
	result, err := c.Check(context.Background(), map[string]any{"hostname": "alias.test", "resolve_type": "A", "resolve_server": "127.0.0.1", "expected_value": "target.test", "timeout": 2.0})
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Fatalf("Check() = %+v, want UP (expected_value matched the CNAME target)", result)
	}
}
