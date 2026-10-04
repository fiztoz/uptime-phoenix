package checker

import (
	"slices"
	"sync"

	"golang.org/x/net/icmp"
)

// pullCheckerTypes is the closed remote-executable pull inventory. Push stays
// local-only until the public push gateway milestone; adding a monitor type
// requires user approval (see AGENTS.md).
var pullCheckerTypes = []string{
	"database", "dns", "docker", "grpc", "http", "mqtt", "ping",
	"rabbitmq", "s3", "snmp", "tcp", "websocket",
}

var pullTypesOnce sync.Once
var registeredPullTypes []string

// RegisteredPullTypes returns the pull monitor types whose checkers are
// actually installed in this build, in stable sorted order. It reflects the
// registry rather than a hardcoded list so a build without a checker never
// claims its capability.
func RegisteredPullTypes() []string {
	pullTypesOnce.Do(func() {
		registeredPullTypes = make([]string, 0, len(pullCheckerTypes))
		for _, kind := range pullCheckerTypes {
			if c, ok := registry[kind]; ok && c != nil && c.Type() == kind {
				registeredPullTypes = append(registeredPullTypes, kind)
			}
		}
		slices.Sort(registeredPullTypes)
	})
	return slices.Clone(registeredPullTypes)
}

// ICMPAvailable reports whether the unprivileged ICMP socket the PingChecker
// needs can be opened on this host. It opens and immediately closes the exact
// "udp4" packet socket pro-bing uses for SetPrivileged(false); no packet is
// sent. On Linux this requires net.ipv4.ping_group_range to cover the runtime
// user; on Windows and default macOS it fails, so ping is honestly withheld.
// IPv6-only unprivileged reachability is deliberately not sufficient: the
// common remote monitor resolves IPv4 targets, and advertising ping without
// udp4 would turn unreachable IPv4 checks into false DOWN alerts.
func ICMPAvailable() bool {
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return false
	}
	return conn.Close() == nil
}

// proxyCapableTypes are the pull checkers whose Check reads the scheduler's
// "_proxy" fragment. Every other type dials the target directly, so a proxy
// on one of those monitors would be silently ignored.
var proxyCapableTypes = []string{"http", "s3"}

// supportedProxyProtocols are the schemes buildProxyTransport can dial.
// socks4 stays excluded: the vendored dialer has no SOCKS4 client.
var supportedProxyProtocols = []string{"http", "https", "socks5"}

// ProxyCapable reports whether a monitor type honors a configured proxy.
func ProxyCapable(kind string) bool {
	return slices.Contains(proxyCapableTypes, kind)
}

// SupportedProxyProtocol reports whether the compiled dialer implements protocol.
func SupportedProxyProtocol(protocol string) bool {
	return slices.Contains(supportedProxyProtocols, protocol)
}

// ProxyCapabilityName is the hello name for one supported proxy protocol.
func ProxyCapabilityName(protocol string) string {
	return "proxy." + protocol + ".v1"
}

// ProxyCapabilityNames returns the hello names for every compiled proxy protocol.
func ProxyCapabilityNames() []string {
	out := make([]string, len(supportedProxyProtocols))
	for i, protocol := range supportedProxyProtocols {
		out[i] = ProxyCapabilityName(protocol)
	}
	return out
}
