package provisioning

import (
	"context"
	"slices"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// SSHProbeCleanupSessionFactory builds one cleanup host session per admitted
// cleanup over a transient deployment key. It stores no credential and
// performs no SSH I/O until the cleanup runs.
type SSHProbeCleanupSessionFactory struct {
	policy  probe.EndpointPolicy
	actions ProbeHostActionAuthorizer
}

var _ ports.ProbeCleanupSessionFactory = (*SSHProbeCleanupSessionFactory)(nil)

// NewSSHProbeCleanupSessionFactory binds the destination policy and the
// cleanup action authorizer.
func NewSSHProbeCleanupSessionFactory(policy probe.EndpointPolicy, actions ProbeHostActionAuthorizer) (*SSHProbeCleanupSessionFactory, error) {
	if imageSnapshotNil(actions) {
		return nil, domain.ErrValidation
	}
	policy.AllowedCIDRs = slices.Clone(policy.AllowedCIDRs)
	return &SSHProbeCleanupSessionFactory{policy: policy, actions: actions}, nil
}

// NewCleanupSession parses the borrowed key without retaining its buffer and
// returns the session with the key's PUBLIC fingerprint.
func (f *SSHProbeCleanupSessionFactory) NewCleanupSession(ctx context.Context, privateKey []byte, hostFingerprint string) (ports.ProbeCleanupHostSession, string, error) {
	if f == nil || imageSnapshotNil(ctx) {
		return nil, "", domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, "", domain.ErrProbeSSHInput
	}
	clientFingerprint := ssh.FingerprintSHA256(signer.PublicKey())
	sender, err := NewSSHBinaryStageSender(ctx, privateKey, hostFingerprint, clientFingerprint, f.policy)
	if err != nil {
		return nil, "", domain.ErrProbeSSHInput
	}
	return &sshProbeCleanupSession{sender: sender, actions: f.actions}, clientFingerprint, nil
}

type sshProbeCleanupSession struct {
	sender  *SSHBinaryStageSender
	actions ProbeHostActionAuthorizer
}

// Cleanup runs the authorized cleanup action. A helper refusal after its
// grant cannot be told apart from a partial removal, so it is unknown.
func (s *sshProbeCleanupSession) Cleanup(ctx context.Context, intent domain.ProbeHostActionIntent) domain.ProbeInstallResourceOutcome {
	if intent.Kind != domain.ProbeHostActionCleanup {
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"}
	}
	ack, err := s.sender.SendHostAction(ctx, intent, s.actions)
	if err == nil && ack.Intent == intent {
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}
	}
	if err == nil {
		err = ErrSSHBinaryStageUnknown
	}
	return probeActivationSendOutcome(err)
}
