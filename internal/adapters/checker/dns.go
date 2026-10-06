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
type DNSChecker struct {
	exchangeFunc dnsExchange
}

// dnsExchange is an instance-scoped seam for exercising DNS checks against a
// local server while retaining the configured resolver address contract.
type dnsExchange func(context.Context, *dns.Client, *dns.Msg, string) (*dns.Msg, time.Duration, error)

// exchange performs a DNS query with the production client unless a test
// supplies an instance-scoped exchange function.
func (checker DNSChecker) exchange(ctx context.Context, client *dns.Client, msg *dns.Msg, address string) (*dns.Msg, time.Duration, error) {
	if checker.exchangeFunc != nil {
		return checker.exchangeFunc(ctx, client, msg, address)
	}
	return client.ExchangeContext(ctx, msg, address)
}

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

func (checker DNSChecker) Check(ctx context.Context, c map[string]any) (ports.CheckResult, error) {
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
	resp, rtt, err := checker.exchange(ctx, client, msg, resolveServer+":53")
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

	// --- Determine evidence for the requested record type (issue #53) ---
	evidence, scanned := dnsEvidence(resp, dnsType, dns.Fqdn(hostname))

	if len(evidence) == 0 {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latency,
			Message:   fmt.Sprintf("no %s records found for %s", resolveType, hostname),
		}, nil
	}

	// --- Extract the first evidence value as a string ---
	answerValue := extractAnswerValue(evidence[0])

	// --- Expected value assertion (scans every considered record, so CNAME
	// targets inside a chain answer keep matching) ---
	if expectedValue != "" {
		found := false
		for _, a := range scanned {
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
				Message:   fmt.Sprintf("expected value %q not found in %d %s record(s) for %s", expectedValue, len(scanned), resolveType, hostname),
			}, nil
		}
	}

	// --- Build metadata ---
	metadata := map[string]string{
		"answer_count": fmt.Sprintf("%d", len(evidence)),
		"rtt_ms":       fmt.Sprintf("%d", rtt.Milliseconds()),
	}
	if len(evidence) > 1 {
		metadata["all_answers"] = answerSummary(evidence)
	}

	// --- Build message ---
	var msgParts []string
	msgParts = append(msgParts, fmt.Sprintf("%s %s → %s", resolveType, hostname, answerValue))
	if len(evidence) > 1 {
		msgParts = append(msgParts, fmt.Sprintf("(%d records)", len(evidence)))
	}

	return ports.CheckResult{
		Status:    domain.StatusUp,
		LatencyMs: latency,
		Message:   strings.Join(msgParts, " "),
		Metadata:  metadata,
	}, nil
}

// dnsEvidence returns the records that prove the requested record type exists,
// plus every record scanned while looking for them (used for expected-value
// matching).
//
// Evidence rules (issue #53): a NOERROR response with an empty Answer section is
// a NODATA answer — its authority SOA describes negative caching and is never
// evidence that the requested record exists.
//   - records of the requested type in the Answer section are evidence;
//   - a bare CNAME in the Answer section is evidence for any other requested
//     type (the name resolves through an alias);
//   - for SOA and NS checks only, records of the requested type in the
//     authority section owned by the queried name are evidence (zone-apex
//     records and delegations are delivered there).
func dnsEvidence(resp *dns.Msg, dnsType uint16, questionName string) (evidence, scanned []dns.RR) {
	scanned = append([]dns.RR{}, resp.Answer...)
	evidence = filterRR(resp.Answer, dnsType, "")
	if len(evidence) == 0 && dnsType != dns.TypeCNAME {
		evidence = filterRR(resp.Answer, dns.TypeCNAME, "")
	}
	if dnsType == dns.TypeSOA || dnsType == dns.TypeNS {
		authority := filterRR(resp.Ns, dnsType, questionName)
		scanned = append(scanned, authority...)
		if len(evidence) == 0 {
			evidence = authority
		}
	}
	return evidence, scanned
}

// filterRR returns records of the given type, optionally restricted to records
// owned by owner (case-insensitive canonical name match).
func filterRR(records []dns.RR, rrtype uint16, owner string) []dns.RR {
	out := make([]dns.RR, 0, len(records))
	for _, rr := range records {
		if rr.Header().Rrtype != rrtype {
			continue
		}
		if owner != "" && !strings.EqualFold(dns.Fqdn(rr.Header().Name), dns.Fqdn(owner)) {
			continue
		}
		out = append(out, rr)
	}
	return out
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
