package provisioning

import (
	"context"
	"errors"
	"slices"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// SSHProbeActivationSessionFactory builds one activation host session per
// admitted activation over a transient deployment key. It stores no
// credential and performs no SSH I/O until a step runs.
type SSHProbeActivationSessionFactory struct {
	policy  probe.EndpointPolicy
	actions ProbeHostActionAuthorizer
}

var _ ports.ProbeActivationSessionFactory = (*SSHProbeActivationSessionFactory)(nil)

// NewSSHProbeActivationSessionFactory binds the destination policy and the
// activation action authorizer.
func NewSSHProbeActivationSessionFactory(policy probe.EndpointPolicy, actions ProbeHostActionAuthorizer) (*SSHProbeActivationSessionFactory, error) {
	if imageSnapshotNil(actions) {
		return nil, domain.ErrValidation
	}
	policy.AllowedCIDRs = slices.Clone(policy.AllowedCIDRs)
	return &SSHProbeActivationSessionFactory{policy: policy, actions: actions}, nil
}

// NewActivationSession parses the borrowed key without retaining its buffer
// and returns the session with the key's PUBLIC fingerprint.
func (f *SSHProbeActivationSessionFactory) NewActivationSession(ctx context.Context, privateKey []byte, hostFingerprint string) (ports.ProbeActivationHostSession, string, error) {
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
	return &sshProbeActivationSession{sender: sender, actions: f.actions}, clientFingerprint, nil
}

type sshProbeActivationSession struct {
	sender  *SSHBinaryStageSender
	actions ProbeHostActionAuthorizer
}

// ProbeInit runs the authorized probe_init action. The returned token is the
// caller's to hold in memory only.
func (s *sshProbeActivationSession) ProbeInit(ctx context.Context, intent domain.ProbeHostActionIntent) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, string) {
	result, err := s.sender.SendProbeInit(ctx, intent, s.actions)
	if err != nil {
		return probeActivationSendOutcome(err), domain.ProbeActivationFacts{}, ""
	}
	return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, result.Facts, result.EnrollmentToken
}

// ServiceStart runs the authorized service_start action.
func (s *sshProbeActivationSession) ServiceStart(ctx context.Context, intent domain.ProbeHostActionIntent) domain.ProbeInstallResourceOutcome {
	if intent.Kind != domain.ProbeActivationServiceStart {
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

// probeActivationSendOutcome maps sender errors exactly like the install
// executors: only Unknown is uncertain; everything else reached no stdin.
func probeActivationSendOutcome(err error) domain.ProbeInstallResourceOutcome {
	if errors.Is(err, ErrSSHBinaryStageUnknown) {
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceUnknown, ErrorCode: "outcome_unknown"}
	}
	return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"}
}
