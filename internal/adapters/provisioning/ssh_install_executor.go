package provisioning

import (
	"context"
	"errors"
	"strings"

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
	// artifact stages the pinned binary (systemd); archive is the closed
	// image archive image_cache streams (Docker). Exactly one is set.
	artifact *SSHArtifactDirectoryExecutor
	archive  ports.PreparedProbeImageArchive
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

// NewSSHProbeDockerInstallExecutor binds the collaborators of a Docker
// install: the sender, the closed image archive image_cache streams and the
// host-action authorizer. It performs no I/O.
func NewSSHProbeDockerInstallExecutor(sender *SSHBinaryStageSender, archive ports.PreparedProbeImageArchive, actions ProbeHostActionAuthorizer) (*SSHProbeInstallExecutor, error) {
	if sender == nil || imageSnapshotNil(archive) || imageSnapshotNil(actions) || !archive.Descriptor().Valid() {
		return nil, ErrSSHBinaryStageInput
	}
	return &SSHProbeInstallExecutor{archive: archive, sender: sender, actions: actions}, nil
}

// ApplyResource dispatches by kind and returns only after the action joins.
func (e *SSHProbeInstallExecutor) ApplyResource(ctx context.Context, operation domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
	notStarted := domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"}
	if e == nil || e.sender == nil || imageSnapshotNil(e.actions) || (e.artifact == nil) == imageSnapshotNil(e.archive) {
		return notStarted, nil
	}
	docker := operation.Identity.Mode == domain.ProbeInstallDocker
	if docker != (e.artifact == nil) {
		// A binary executor never runs a Docker install, nor the reverse.
		return notStarted, nil
	}
	if kind == artifactDirectoryKind {
		return e.artifact.ApplyResource(ctx, operation, kind)
	}
	intent, ok := hostActionIntentFromReceipt(operation, kind)
	if !ok {
		return notStarted, nil
	}
	var ack SSHHostActionAck
	var err error
	if kind == domain.ProbeHostActionImageCache {
		d := e.archive.Descriptor()
		intent.Payload = domain.ProbeHostActionPayload{SHA256: d.ArchiveSHA256, Size: d.ArchiveSize, LayoutIndexSHA256: d.LayoutIndexSHA256}
		if !intent.Valid() || d.Content.Selection != operation.Identity.Image {
			return notStarted, nil
		}
		ack, err = e.sender.SendImageCache(ctx, intent, e.archive, e.actions)
	} else {
		ack, err = e.sender.SendHostAction(ctx, intent, e.actions)
	}
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
// the acknowledged reservation receipt: a running systemd or Docker operation whose
// layout equals the identity-derived one, with the kind's resource in flight
// and every earlier resource applied.
func hostActionIntentFromReceipt(operation domain.ProbeInstallOperation, kind string) (domain.ProbeHostActionIntent, bool) {
	var zero domain.ProbeHostActionIntent
	if operation.State != domain.ProbeInstallRunning ||
		(operation.Identity.Mode != domain.ProbeInstallSystemd && operation.Identity.Mode != domain.ProbeInstallDocker) {
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
	if kind == domain.ProbeHostActionImageCache {
		// The payload is filled by the caller from its archive; a
		// placeholder lets the intent validate its resource here.
		probe := intent
		probe.Payload = domain.ProbeHostActionPayload{SHA256: strings.Repeat("0", 64), Size: 1, LayoutIndexSHA256: strings.Repeat("0", 64)}
		if spec, ok := probe.Resource(); !ok || spec != layout[index] {
			return zero, false
		}
		return intent, true
	}
	if spec, ok := intent.Resource(); !ok || spec != layout[index] {
		return zero, false
	}
	return intent, true
}
