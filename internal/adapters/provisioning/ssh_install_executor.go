package provisioning

import (
	"context"
	"errors"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// SSHProbeInstallExecutor applies every systemd resource of one operation:
// artifact_directory through SSHArtifactDirectoryExecutor, and data_directory,
// unit_file and service through authorized host actions. It borrows all of its
// collaborators. Reports follow the artifact executor: applied after an exact
// acknowledgement, not_started when nothing reached the helper's stdin, and
// unknown otherwise. It never returns an error.
type SSHProbeInstallExecutor struct {
	artifact *SSHArtifactDirectoryExecutor
	sender   *SSHBinaryStageSender
	actions  ProbeHostActionAuthorizer
}

var _ ports.ProbeInstallResourceExecutor = (*SSHProbeInstallExecutor)(nil)

// NewSSHProbeInstallExecutor binds the borrowed collaborators. It performs no I/O.
func NewSSHProbeInstallExecutor(sender *SSHBinaryStageSender, binary ports.PreparedProbeBinary, stage ProbeBinaryStageAuthorizer, actions ProbeHostActionAuthorizer) (*SSHProbeInstallExecutor, error) {
	artifact, err := NewSSHArtifactDirectoryExecutor(sender, binary, stage)
	if err != nil || imageSnapshotNil(actions) {
		return nil, ErrSSHBinaryStageInput
	}
	return &SSHProbeInstallExecutor{artifact: artifact, sender: sender, actions: actions}, nil
}

// ApplyResource dispatches by kind and returns only after the action joins.
func (e *SSHProbeInstallExecutor) ApplyResource(ctx context.Context, operation domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
	notStarted := domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"}
	if e == nil || e.artifact == nil || e.sender == nil || imageSnapshotNil(e.actions) {
		return notStarted, nil
	}
	if kind == artifactDirectoryKind {
		return e.artifact.ApplyResource(ctx, operation, kind)
	}
	intent, ok := hostActionIntentFromReceipt(operation, kind)
	if !ok {
		return notStarted, nil
	}
	ack, err := e.sender.SendHostAction(ctx, intent, e.actions)
	switch {
	case err == nil && ack.Intent == intent:
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, nil
	case err == nil, errors.Is(err, ErrSSHBinaryStageUnknown):
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceUnknown, ErrorCode: "outcome_unknown"}, nil
	default:
		return notStarted, nil
	}
}

// hostActionIntentFromReceipt rebuilds the host-action intent for kind from
// the acknowledged reservation receipt: a running systemd operation whose
// layout equals the identity-derived one, with the kind's resource in flight
// and every earlier resource applied.
func hostActionIntentFromReceipt(operation domain.ProbeInstallOperation, kind string) (domain.ProbeHostActionIntent, bool) {
	var zero domain.ProbeHostActionIntent
	if operation.State != domain.ProbeInstallRunning || operation.Identity.Mode != domain.ProbeInstallSystemd {
		return zero, false
	}
	layout := operation.Identity.Resources()
	if len(layout) == 0 || len(layout) != len(operation.Resources) {
		return zero, false
	}
	index := -1
	for i := range layout {
		if operation.Resources[i].Spec != layout[i] {
			return zero, false
		}
		if layout[i].Kind == kind {
			index = i
		}
	}
	if index < 0 {
		return zero, false
	}
	resource := operation.Resources[index]
	if resource.State != domain.ProbeInstallResourceInFlight || resource.AttemptID == "" {
		return zero, false
	}
	for i := 0; i < index; i++ {
		if operation.Resources[i].State != domain.ProbeInstallResourceApplied {
			return zero, false
		}
	}
	intent := domain.ProbeHostActionIntent{Identity: operation.Identity, OwnerID: operation.OwnerID, AttemptID: resource.AttemptID, Kind: kind}
	if spec, ok := intent.Resource(); !ok || spec != layout[index] {
		return zero, false
	}
	return intent, true
}
