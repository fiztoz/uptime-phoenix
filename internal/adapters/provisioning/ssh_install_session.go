package provisioning

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// SSHProbeInstallSessionFactory builds one install session per admitted
// operation: a transient-key SSH sender, the pinned artifact prepared from the
// operator's artifact directory, and SSHProbeInstallExecutor bound to the two
// authorization services. It holds the artifact directory open for the
// process lifetime; it stores no credential, signer or artifact itself.
type SSHProbeInstallSessionFactory struct {
	policy    probe.EndpointPolicy
	artifacts *os.Root
	preparer  *ProbeBinaryPreparer
	stage     ProbeBinaryStageAuthorizer
	actions   ProbeHostActionAuthorizer
}

var _ ports.ProbeInstallSessionFactory = (*SSHProbeInstallSessionFactory)(nil)

// NewSSHProbeInstallSessionFactory opens the artifact directory as a rooted
// handle, so release names can never escape it, and captures a private copy of
// the destination policy. Prepared snapshots use the OS temporary directory.
func NewSSHProbeInstallSessionFactory(artifactDir string, policy probe.EndpointPolicy, stage ProbeBinaryStageAuthorizer, actions ProbeHostActionAuthorizer) (*SSHProbeInstallSessionFactory, error) {
	if artifactDir == "" || imageSnapshotNil(stage) || imageSnapshotNil(actions) {
		return nil, domain.ErrValidation
	}
	root, err := os.OpenRoot(artifactDir)
	if err != nil {
		return nil, errors.New("probe install artifact directory unavailable")
	}
	policy.AllowedCIDRs = slices.Clone(policy.AllowedCIDRs)
	return &SSHProbeInstallSessionFactory{policy: policy, artifacts: root, preparer: NewProbeBinaryPreparer(""), stage: stage, actions: actions}, nil
}

// Close releases the artifact directory handle.
func (f *SSHProbeInstallSessionFactory) Close() error {
	if f == nil || f.artifacts == nil {
		return nil
	}
	return f.artifacts.Close()
}

// NewInstallSession parses the borrowed key without retaining its buffer,
// prepares and verifies the pinned release file, and returns the session with
// the key's PUBLIC fingerprint. It performs no SSH I/O. Key and trust input
// failures are domain.ErrProbeSSHInput; artifact failures are
// domain.ErrProbeInstallArtifact.
func (f *SSHProbeInstallSessionFactory) NewInstallSession(ctx context.Context, privateKey []byte, hostFingerprint string, pin domain.ProbeBinaryPin) (ports.ProbeInstallSession, string, error) {
	if f == nil || f.artifacts == nil || imageSnapshotNil(ctx) {
		return nil, "", domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if !pin.Valid() {
		return nil, "", domain.ErrProbeInstallArtifact
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
	input, err := f.artifacts.Open(pin.ReleaseName())
	if err != nil {
		return nil, "", domain.ErrProbeInstallArtifact
	}
	if info, err := input.Stat(); err != nil || !info.Mode().IsRegular() {
		_ = input.Close()
		return nil, "", domain.ErrProbeInstallArtifact
	}
	prepared, err := f.preparer.PrepareBinary(ctx, pin, input) // consumes input
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, "", ctxErr
		}
		return nil, "", domain.ErrProbeInstallArtifact
	}
	executor, err := NewSSHProbeInstallExecutor(sender, prepared, f.stage, f.actions)
	if err != nil {
		_ = prepared.Close(context.WithoutCancel(ctx))
		return nil, "", domain.ErrInternal
	}
	return &sshProbeInstallSession{SSHProbeInstallExecutor: executor, prepared: prepared}, clientFingerprint, nil
}

// sshProbeInstallSession owns one prepared snapshot until Close.
type sshProbeInstallSession struct {
	*SSHProbeInstallExecutor
	prepared ports.PreparedProbeBinary
	once     sync.Once
	err      error
}

// Preflight runs the fixed install preflight over the session's pinned
// transport. It never authenticates to an unverified host.
func (s *sshProbeInstallSession) Preflight(ctx context.Context, host domain.ProbeSSHHost) (domain.ProbeInstallHostReport, error) {
	return s.sender.insp.host.InspectInstallHost(ctx, host)
}

// Close disposes the prepared snapshot exactly once.
func (s *sshProbeInstallSession) Close(ctx context.Context) error {
	s.once.Do(func() { s.err = s.prepared.Close(ctx) })
	return s.err
}
