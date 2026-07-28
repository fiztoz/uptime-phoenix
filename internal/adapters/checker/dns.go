package checker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// DNSChecker performs DNS record queries using miekg/dns.
// Config fields:
//
//	hostname        (string, required) — the domain to query
//	resolve_type    (string, optional, default "A") — record type: A, AAAA, CNAME, MX, TXT, NS, SRV, SOA, PTR, CAA
//	resolve_server   (string, optional, default "8.8.8.8") — DNS server to query (IP, no port)
//	expected_value  (string, optional) — if set, at least one answer must contain this value
//	timeout         (float64, optional, default 10) — query timeout in seconds
type DNSChecker struct{}

func init() { Register(DNSChecker{}) }

func (DNSChecker) Type() string { return "dns" }

func (DNSChecker) Validate(c map[string]any) error {
	hostname, _ := c["hostname"].(string)
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return fmt.Errorf("hostname is required")
	}

	// Validate resolve_type if provided.
	if rt, ok := c["resolve_type"].(string); ok && rt != "" {
		if _, ok := dnsTypeFromString(rt); !ok {
			return fmt.Errorf("unsupported resolve_type %q, supported: A, AAAA, CNAME, MX, TXT, NS, SRV, SOA, PTR, CAA", rt)
		}
	}

	// Validate resolve_server is an IP-like string if provided.
	if rs, ok := c["resolve_server"].(string); ok && rs != "" {
		rs = strings.TrimSpace(rs)
		if rs == "" {
			return fmt.Errorf("resolve_server must not be empty")
		}
	}

	// Validate timeout if provided.
	if t, ok := c["timeout"].(float64); ok {
		if t <= 0 {
			return fmt.Errorf("timeout must be positive")
		}
	}

	return nil
}

func (DNSChecker) Check(ctx context.Context, c map[string]any) (ports.CheckResult, error) {
	// --- Extract config with defaults ---
	hostname, _ := c["hostname"].(string)
	hostname = strings.TrimSpace(hostname)

	resolveType := "A"
	if rt, ok := c["resolve_type"].(string); ok && rt != "" {
		resolveType = strings.ToUpper(strings.TrimSpace(rt))
	}

	resolveServer := "8.8.8.8"
	if rs, ok := c["resolve_server"].(string); ok && rs != "" {
		resolveServer = strings.TrimSpace(rs)
	}

	expectedValue, _ := c["expected_value"].(string)
	expectedValue = strings.TrimSpace(expectedValue)

	timeoutSec := 10.0
	if t, ok := c["timeout"].(float64); ok && t > 0 {
		timeoutSec = t
	}

	// --- Map resolve type to DNS type constant ---
	dnsType, ok := dnsTypeFromString(resolveType)
	if !ok {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("unsupported resolve_type %q", resolveType),
		}, nil
	}

	// --- Set up context with timeout ---
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec*float64(time.Second)))
	defer cancel()

	// --- Build DNS message ---
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(hostname), dnsType)
	msg.RecursionDesired = true

	// --- Create client with timeout on dialer ---
	client := &dns.Client{
		Timeout: time.Duration(timeoutSec * float64(time.Second)),
	}

	// --- Execute query ---
	start := time.Now()
	resp, rtt, err := client.ExchangeContext(ctx, msg, resolveServer+":53")
	latency := time.Since(start).Milliseconds()

	if err != nil {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latency,
			Message:   fmt.Sprintf("DNS query failed: %s", err),
		}, nil
	}

	// --- Check response code ---
	if resp.Rcode != dns.RcodeSuccess {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latency,
			Message:   fmt.Sprintf("DNS rcode %d (%s) for %s %s", resp.Rcode, dns.RcodeToString[resp.Rcode], resolveType, hostname),
		}, nil
	}

	// --- Parse answers ---
	// Combine Answer, Ns, and Extra sections for record types that use them (e.g., SOA, NS).
	answers := resp.Answer
	if len(answers) == 0 {
		answers = resp.Ns
	}

	if len(answers) == 0 {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latency,
			Message:   fmt.Sprintf("no %s records found for %s", resolveType, hostname),
		}, nil
	}

	// --- Extract the first answer value as a string ---
	answerValue := extractAnswerValue(answers[0])

	// --- Expected value assertion ---
	if expectedValue != "" {
		found := false
		for _, a := range answers {
			val := extractAnswerValue(a)
			if strings.Contains(val, expectedValue) {
				found = true
				break
			}
		}
		if !found {
			return ports.CheckResult{
				Status:    domain.StatusDown,
				LatencyMs: latency,
				Message:   fmt.Sprintf("expected value %q not found in %d %s record(s) for %s", expectedValue, len(answers), resolveType, hostname),
			}, nil
		}
	}

	// --- Build metadata ---
	metadata := map[string]string{
		"answer_count": fmt.Sprintf("%d", len(answers)),
		"rtt_ms":       fmt.Sprintf("%d", rtt.Milliseconds()),
	}
	if len(answers) > 1 {
		metadata["all_answers"] = answerSummary(answers)
	}

	// --- Build message ---
	var msgParts []string
	msgParts = append(msgParts, fmt.Sprintf("%s %s → %s", resolveType, hostname, answerValue))
	if len(answers) > 1 {
		msgParts = append(msgParts, fmt.Sprintf("(%d records)", len(answers)))
	}

	return ports.CheckResult{
		Status:    domain.StatusUp,
		LatencyMs: latency,
		Message:   strings.Join(msgParts, " "),
		Metadata:  metadata,
	}, nil
}

// dnsTypeFromString maps a string record type to its dns.Type constant.
func dnsTypeFromString(s string) (uint16, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "A":
		return dns.TypeA, true
	case "AAAA":
		return dns.TypeAAAA, true
	case "CNAME":
		return dns.TypeCNAME, true
	case "MX":
		return dns.TypeMX, true
	case "TXT":
		return dns.TypeTXT, true
	case "NS":
		return dns.TypeNS, true
	case "SRV":
		return dns.TypeSRV, true
	case "SOA":
		return dns.TypeSOA, true
	case "PTR":
		return dns.TypePTR, true
	case "CAA":
		return dns.TypeCAA, true
	default:
		return 0, false
	}
}

// extractAnswerValue returns a human-readable string for a DNS answer record.
func extractAnswerValue(rr dns.RR) string {
	switch v := rr.(type) {
	case *dns.A:
		return v.A.String()
	case *dns.AAAA:
		return v.AAAA.String()
	case *dns.CNAME:
		return v.Target
	case *dns.MX:
		return fmt.Sprintf("%s (priority %d)", v.Mx, v.Preference)
	case *dns.TXT:
		return strings.Join(v.Txt, " ")
	case *dns.NS:
		return v.Ns
	case *dns.SRV:
		return fmt.Sprintf("%s:%d (priority %d, weight %d)", v.Target, v.Port, v.Priority, v.Weight)
	case *dns.SOA:
		return fmt.Sprintf("%s %s (serial %d)", v.Ns, v.Mbox, v.Serial)
	case *dns.PTR:
		return v.Ptr
	case *dns.CAA:
		return fmt.Sprintf("%s %d %q", v.Tag, v.Flag, v.Value)
	default:
		return rr.String()
	}
}

// answerSummary returns a semicolon-separated list of all answer values.
func answerSummary(answers []dns.RR) string {
	vals := make([]string, 0, len(answers))
	for _, a := range answers {
		vals = append(vals, extractAnswerValue(a))
	}
	return strings.Join(vals, "; ")
}
