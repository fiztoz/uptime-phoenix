package provisioning

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestSSHPreflightFreezesCallerPolicy(t *testing.T) {
	f := serveSSH(t, nil)
	policy := probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	i, err := NewSSHHostInspector(t.Context(), f.clientKey, SSHHostTrust{Fingerprint: ssh.FingerprintSHA256(f.hostKey.PublicKey())}, policy)
	if err != nil {
		t.Fatal(err)
	}
	policy.AllowedCIDRs[0] = netip.MustParsePrefix("10.0.0.0/8")
	if _, err := i.InspectHost(t.Context(), domain.ProbeSSHHost{Address: f.address, User: "deploy"}); err != nil {
		t.Fatal("caller mutation changed frozen destination policy")
	}
}

func TestSSHPreflightCanceledAndInvalidRequestsDoNotDial(t *testing.T) {
	key, signer := deploymentKey(t)
	i, err := NewSSHHostInspector(t.Context(), key, SSHHostTrust{Fingerprint: ssh.FingerprintSHA256(signer.PublicKey())}, probe.EndpointPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	i.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		t.Fatal("invalid/canceled request resolved DNS")
		return nil, nil
	}
	i.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("invalid/canceled request dialed")
		return nil, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewSSHHostInspector(ctx, nil, SSHHostTrust{KnownHostsFile: "not-opened"}, probe.EndpointPolicy{}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled constructor did not refuse before trust/key work")
	}
	if _, err := i.InspectHost(ctx, domain.ProbeSSHHost{Address: "edge.example:22", User: "deploy"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := i.InspectHost(t.Context(), domain.ProbeSSHHost{Address: "edge.example:22;id", User: "deploy"}); !errors.Is(err, domain.ErrProbeSSHInput) {
		t.Fatal(err)
	}
}

func TestSSHPreflightDNSFailureIsSanitized(t *testing.T) {
	key, signer := deploymentKey(t)
	i, err := NewSSHHostInspector(t.Context(), key, SSHHostTrust{Fingerprint: ssh.FingerprintSHA256(signer.PublicKey())}, probe.EndpointPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	i.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, errors.New("sensitive DNS diagnostic")
	}
	if _, err := i.InspectHost(t.Context(), domain.ProbeSSHHost{Address: "edge.example:22", User: "deploy"}); err != domain.ErrProbeSSHConnect {
		t.Fatal("DNS failure was not sanitized")
	}
}

func TestSSHPreflightSharedOutputBudget(t *testing.T) {
	closed := false
	o := &discoveryOutput{close: func() error { closed = true; return nil }}
	if n, err := (discoveryWriter{output: o, capture: true}).Write(make([]byte, maxDiscoverySize/2)); err != nil || n != maxDiscoverySize/2 {
		t.Fatal("valid stdout rejected")
	}
	if n, err := (discoveryWriter{output: o}).Write(make([]byte, maxDiscoverySize/2)); err != nil || n != maxDiscoverySize/2 {
		t.Fatal("valid stderr rejected")
	}
	if _, err := (discoveryWriter{output: o}).Write([]byte("x")); err != domain.ErrProbeSSHDiscovery || !closed || !o.exceeded || o.stdout.Len() != maxDiscoverySize/2 {
		t.Fatal("stdout+stderr did not share one bounded budget and close on overflow")
	}
}
