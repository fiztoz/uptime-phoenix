package provisioning

import (
	"context"
	"errors"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const artifactDirectoryKind = "artifact_directory"

// SSHArtifactDirectoryExecutor applies only the systemd artifact_directory
// resource of one install operation, by authorized SSH staging. It borrows the
// sender, the prepared binary and the authorizer; it closes none of them.
//
// Reports are joined and conservative:
//   - applied: an authorized helper acknowledged a fresh stage of exactly this
//     attempt. The attempt was irreversibly admitted in the DB first, and the
//     helper verified the grant for its own nonce and host key.
//   - not_started: nothing reached the remote helper's stdin. This covers every
//     pre-exec refusal, ErrSSHBinaryStageNotStaged, a kind other than
//     artifact_directory, and an operation whose reservation does not match.
//   - outcome_unknown: anything after the first stdin byte.
//
// It never returns an error: the runner records any error as unknown, which
// would misreport a resource that was provably never touched.
type SSHArtifactDirectoryExecutor struct {
	sender     *SSHBinaryStageSender
	binary     ports.PreparedProbeBinary
	authorizer ProbeBinaryStageAuthorizer
}

var _ ports.ProbeInstallResourceExecutor = (*SSHArtifactDirectoryExecutor)(nil)

// NewSSHArtifactDirectoryExecutor binds borrowed collaborators for one
// operation's artifact. It performs no I/O.
func NewSSHArtifactDirectoryExecutor(sender *SSHBinaryStageSender, binary ports.PreparedProbeBinary, authorizer ProbeBinaryStageAuthorizer) (*SSHArtifactDirectoryExecutor, error) {
	if sender == nil || imageSnapshotNil(binary) || imageSnapshotNil(authorizer) {
		return nil, ErrSSHBinaryStageInput
	}
	return &SSHArtifactDirectoryExecutor{sender: sender, binary: binary, authorizer: authorizer}, nil
}

// ApplyResource stages the operation's pinned binary for its reserved
// artifact_directory attempt and returns only after the send has joined.
func (e *SSHArtifactDirectoryExecutor) ApplyResource(ctx context.Context, operation domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
	notStarted := domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"}
	if e == nil || kind != artifactDirectoryKind {
		return notStarted, nil
	}
	intent, ok := artifactDirectoryIntent(operation)
	if !ok || imageSnapshotNil(e.binary) || e.binary.Pin() != intent.Identity.Binary {
		return notStarted, nil
	}
	ack, err := e.sender.SendAuthorized(ctx, intent, e.binary, e.authorizer)
	switch {
	case err == nil && ack.Intent == intent:
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, nil
	case err == nil, errors.Is(err, ErrSSHBinaryStageUnknown):
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceUnknown, ErrorCode: "outcome_unknown"}, nil
	default:
		// ErrSSHBinaryStageNotStaged and every pre-exec sentinel (input,
		// trust, destination, connect, storage, context) precede any stdin.
		return notStarted, nil
	}
}

// artifactDirectoryIntent rebuilds the stage intent from the acknowledged
// reservation receipt: the in-flight artifact_directory attempt of a running
// systemd operation, with every predecessor applied and the complete resource
// layout equal to the one derived from the immutable identity.
func artifactDirectoryIntent(operation domain.ProbeInstallOperation) (domain.ProbeBinaryStageIntent, bool) {
	var zero domain.ProbeBinaryStageIntent
	if operation.State != domain.ProbeInstallRunning || operation.Identity.Mode != domain.ProbeInstallSystemd || len(operation.Resources) < 2 {
		return zero, false
	}
	layout := operation.Identity.Resources()
	if len(layout) != len(operation.Resources) {
		return zero, false
	}
	for i := range layout {
		if operation.Resources[i].Spec != layout[i] {
			return zero, false
		}
	}
	resource := operation.Resources[1]
	if resource.Spec.Kind != artifactDirectoryKind || resource.State != domain.ProbeInstallResourceInFlight || resource.AttemptID == "" ||
		operation.Resources[0].State != domain.ProbeInstallResourceApplied {
		return zero, false
	}
	intent := domain.ProbeBinaryStageIntent{Identity: operation.Identity, OwnerID: operation.OwnerID, AttemptID: resource.AttemptID}
	if !intent.Valid() || intent.Directory() != resource.Spec.Reference {
		return zero, false
	}
	return intent, true
}
