package provisioning

import (
	"context"
	"slices"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// SSHHostInspectorFactory parses one transient signer per admitted operation.
// Only an explicit host pin is accepted; no key/known-hosts path is accepted.
// It starts no network work and stores no credential or parsed signer itself.
type SSHHostInspectorFactory struct{ policy probe.EndpointPolicy }

var _ ports.ProbeHostInspectorFactory = (*SSHHostInspectorFactory)(nil)

// NewSSHHostInspectorFactory captures a private copy of the destination policy.
func NewSSHHostInspectorFactory(policy probe.EndpointPolicy) *SSHHostInspectorFactory {
	policy.AllowedCIDRs = slices.Clone(policy.AllowedCIDRs)
	return &SSHHostInspectorFactory{policy: policy}
}

// NewInspector parses the borrowed key synchronously without retaining its
// buffer, returning only the PUBLIC authentication-key fingerprint alongside
// an ephemeral inspector. Existing trust-before-auth and numeric-dial policy,
// fixed commands, timeouts and output bounds apply unchanged.
func (f *SSHHostInspectorFactory) NewInspector(ctx context.Context, privateKey []byte, hostPin string) (ports.ProbeHostInspector, string, error) {
	inspector, err := NewSSHHostInspector(ctx, privateKey, SSHHostTrust{Fingerprint: hostPin}, f.policy)
	if err != nil {
		return nil, "", err
	}
	return inspector, ssh.FingerprintSHA256(inspector.signer.PublicKey()), nil
}
