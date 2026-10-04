package checker

import (
	"slices"
	"testing"

	"golang.org/x/net/icmp"
)

func TestRegisteredPullTypesReflectsRegistry(t *testing.T) {
	got := RegisteredPullTypes()
	want := []string{
		"database", "dns", "docker", "grpc", "http", "mqtt", "ping",
		"rabbitmq", "s3", "snmp", "tcp", "websocket",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("pull inventory changed: %v", got)
	}
	if slices.Contains(got, "push") {
		t.Fatal("push is local-only and must never be advertised remotely")
	}
	for _, kind := range got {
		if c, ok := Get(kind); !ok || c == nil || c.Type() != kind {
			t.Fatalf("advertised checker not installed: %s", kind)
		}
	}
	// Repeated calls return equal copies, not a shared mutable slice.
	first, second := RegisteredPullTypes(), RegisteredPullTypes()
	if !slices.Equal(first, second) || &first[0] == &second[0] {
		t.Fatal("registered inventory unstable or shared")
	}
}

// TestICMPAvailableMatchesSocketReality proves the advertisement cannot claim
// more than the exact unprivileged socket the ping checker opens. The direct
// ListenPacket call is the same syscall path pro-bing uses for
// SetPrivileged(false).
func TestICMPAvailableMatchesSocketReality(t *testing.T) {
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		if ICMPAvailable() {
			t.Fatal("ICMP advertised although the unprivileged socket cannot open")
		}
		return
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("closing probe socket failed: %v", err)
	}
	if !ICMPAvailable() {
		t.Fatal("unprivileged socket opens but ICMP is not advertised")
	}
}
