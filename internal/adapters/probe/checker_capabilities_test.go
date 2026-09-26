package probe

import (
	"context"
	"slices"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type stubLookupChecker struct{ kind string }

func (c stubLookupChecker) Type() string                  { return c.kind }
func (c stubLookupChecker) Validate(map[string]any) error { return nil }
func (c stubLookupChecker) Check(context.Context, map[string]any) (ports.CheckResult, error) {
	return ports.CheckResult{Status: domain.StatusUp}, nil
}

var allPullCandidates = []string{
	"database", "dns", "docker", "grpc", "http", "mqtt", "ping",
	"rabbitmq", "s3", "snmp", "tcp", "websocket",
}

func fullStubLookup(kind string) (ports.Checker, bool) { return stubLookupChecker{kind}, true }

func TestPullCheckerCapabilitiesDerivesFromRegistry(t *testing.T) {
	// Without a configured resource map, Docker remains withheld even when
	// the checker is compiled in.
	got := PullCheckerCapabilities(allPullCandidates, fullStubLookup, true)
	want := []string{
		"checker.database.v1", "checker.dns.v1", "checker.grpc.v1", "checker.http.v1",
		"checker.mqtt.v1", "checker.ping.v1", "checker.rabbitmq.v1", "checker.s3.v1",
		"checker.snmp.v1", "checker.tcp.v1", "checker.websocket.v1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("full advertisement wrong: %v", got)
	}

	// Without ICMP the ping capability must disappear, never page falsely.
	got = PullCheckerCapabilities(allPullCandidates, fullStubLookup, false)
	if slices.Contains(got, "checker.ping.v1") || len(got) != len(want)-1 {
		t.Fatalf("ping advertised without ICMP or count changed: %v", got)
	}
}

func TestPullCheckerCapabilitiesWithheldTypes(t *testing.T) {
	lookup := func(kind string) (ports.Checker, bool) {
		switch kind {
		case "database":
			return nil, false // not built
		case "snmp":
			return stubLookupChecker{"grpc"}, false // registered under wrong key
		case "tcp":
			return stubLookupChecker{"tcp"}, true
		default:
			return stubLookupChecker{kind}, true
		}
	}
	got := PullCheckerCapabilities(allPullCandidates, lookup, false)
	for _, forbidden := range []string{"checker.docker.v1", "checker.ping.v1", "checker.database.v1", "checker.snmp.v1"} {
		if slices.Contains(got, forbidden) {
			t.Fatalf("capability withheld incorrectly advertised: %s in %v", forbidden, got)
		}
	}
	for _, expected := range []string{"checker.http.v1", "checker.tcp.v1"} {
		if !slices.Contains(got, expected) {
			t.Fatalf("installed capability missing: %s from %v", expected, got)
		}
	}
	// A checker lookup returning a checker with a mismatched Type() is skipped.
	mismatched := func(string) (ports.Checker, bool) { return stubLookupChecker{"http"}, true }
	if got := PullCheckerCapabilities([]string{"tcp"}, mismatched, true); len(got) != 0 {
		t.Fatalf("mismatched checker type advertised: %v", got)
	}
	if got := PullCheckerCapabilities(nil, nil, true); got != nil {
		t.Fatalf("nil lookup advertised: %v", got)
	}
}

func TestProxyCapabilitiesFollowInstalledProxyCheckers(t *testing.T) {
	want := []string{"proxy.http.v1", "proxy.https.v1", "proxy.socks5.v1"}
	if got := ProxyCapabilities(fullStubLookup); !slices.Equal(got, want) {
		t.Fatalf("proxy protocols = %v", got)
	}
	onlyTCP := func(kind string) (ports.Checker, bool) {
		if kind == "tcp" {
			return stubLookupChecker{"tcp"}, true
		}
		return nil, false
	}
	if got := ProxyCapabilities(onlyTCP); got != nil {
		t.Fatalf("proxy advertised without a checker that dials it: %v", got)
	}
	if got := ProxyCapabilities(nil); got != nil {
		t.Fatal("nil lookup advertised proxy protocols")
	}
}
