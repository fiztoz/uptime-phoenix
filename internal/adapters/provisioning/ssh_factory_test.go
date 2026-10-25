package provisioning

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestSSHPreflightFactoryTransientKeyAndFrozenPolicy(t *testing.T) {
	f := serveSSH(t, nil)
	signer, err := ssh.ParsePrivateKey(f.clientKey)
	if err != nil {
		t.Fatal(err)
	}
	policy := probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	factory := NewSSHHostInspectorFactory(policy)
	policy.AllowedCIDRs[0] = netip.MustParsePrefix("10.0.0.0/8")
	inspector, fingerprint, err := factory.NewInspector(t.Context(), f.clientKey, ssh.FingerprintSHA256(f.hostKey.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint != ssh.FingerprintSHA256(signer.PublicKey()) {
		t.Fatal("retry identity is not the PUBLIC client-key fingerprint")
	}
	clear(f.clientKey)
	facts, err := inspector.InspectHost(t.Context(), domain.ProbeSSHHost{Address: f.address, User: "deploy"})
	if err != nil || facts.Kernel != "Linux" || f.commandCalls.Load() != 1 {
		t.Fatalf("borrowed key/policy altered execution: %v", err)
	}
}

func TestSSHPreflightFactoryRefusalsNeverAuthenticate(t *testing.T) {
	for _, mode := range []string{"wrong_pin", "invalid_pin", "invalid_key", "canceled", "default_destination_policy"} {
		t.Run(mode, func(t *testing.T) {
			f := serveSSH(t, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			pin := ssh.FingerprintSHA256(f.hostKey.PublicKey())
			key := f.clientKey
			policy := probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
			want := domain.ErrProbeSSHInput
			switch mode {
			case "wrong_pin":
				_, other := deploymentKey(t)
				pin = ssh.FingerprintSHA256(other.PublicKey())
				want = domain.ErrProbeSSHTrust
			case "invalid_pin":
				pin = "unverified"
			case "invalid_key":
				key = []byte("not a private key")
			case "canceled":
				cancel()
				want = context.Canceled
			case "default_destination_policy":
				policy = probe.EndpointPolicy{}
				want = domain.ErrProbeSSHDestination
			}
			inspector, _, err := NewSSHHostInspectorFactory(policy).NewInspector(ctx, key, pin)
			if err == nil {
				_, err = inspector.InspectHost(ctx, domain.ProbeSSHHost{Address: f.address, User: "deploy"})
			}
			if !errors.Is(err, want) || f.authCalls.Load() != 0 || f.commandCalls.Load() != 0 {
				t.Fatalf("refusal did not precede authentication: %v", err)
			}
		})
	}
}
