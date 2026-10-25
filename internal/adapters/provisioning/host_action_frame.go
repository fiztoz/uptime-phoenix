package provisioning

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeSystemctl runs one fixed-argument systemctl invocation and returns its
// bounded stdout and exit status. Implementations resolve systemctl from a
// fixed absolute path, never PATH, and pass an empty environment.
type ProbeSystemctl interface {
	Run(ctx context.Context, args ...string) (stdout string, exitCode int, err error)
}

const (
	// ProbeHostActionPrefixSize is the PHXACT1 (systemd) or PHXACT2 (Docker)
	// newline magic plus one kind length byte. The kind bytes and the
	// identity header (PHXSTG1 or PHXDKR1) follow.
	ProbeHostActionPrefixSize = 9
	// MaxProbeHostActionKindSize bounds the kind name.
	MaxProbeHostActionKindSize = 32

	hostActionMagic         = "PHXACT1\n"
	hostActionReceiptPrefix = `{"state":"host_action_applied","kind":"`
	hostActionReceiptMiddle = `","marker_sha256":"`
	hostActionReceiptSuffix = "\"}\n"
)

// EncodeProbeHostActionFrame writes the action prefix, the kind and the
// identity header: for systemd the unchanged stage header (v1), for Docker the
// strict Docker header including any payload descriptor (v2). Invalid
// intents return nil.
func EncodeProbeHostActionFrame(intent domain.ProbeHostActionIntent) ([]byte, error) {
	if !intent.Valid() || len(intent.Kind) > MaxProbeHostActionKindSize {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	if intent.Identity.Mode == domain.ProbeInstallDocker {
		return encodeProbeDockerActionFrame(intent)
	}
	header, err := EncodeProbeBinaryStageHeader(hostActionStageIntent(intent))
	if err != nil {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	frame := make([]byte, 0, ProbeHostActionPrefixSize+len(intent.Kind)+len(header))
	frame = append(frame, hostActionMagic...)
	frame = append(frame, byte(len(intent.Kind)))
	frame = append(frame, intent.Kind...)
	return append(frame, header...), nil
}

// ProbeHostActionKindLength reads the exact 9-byte prefix (v1 or v2) and
// returns the kind length N (1..32). It does not inspect anything that
// follows; ProbeHostActionFrameIsDocker tells the versions apart.
func ProbeHostActionKindLength(prefix []byte) (int, error) {
	if len(prefix) != ProbeHostActionPrefixSize ||
		(string(prefix[:len(hostActionMagic)]) != hostActionMagic && string(prefix[:len(hostActionDockerMagic)]) != hostActionDockerMagic) {
		return 0, domain.ErrProbeBinaryStageWriteInput
	}
	n := int(prefix[len(hostActionMagic)])
	if n < 1 || n > MaxProbeHostActionKindSize {
		return 0, domain.ErrProbeBinaryStageWriteInput
	}
	return n, nil
}

// ProbeHostActionFrameIsDocker reports whether a valid 9-byte prefix is a
// v2 (Docker) frame.
func ProbeHostActionFrameIsDocker(prefix []byte) bool {
	return len(prefix) == ProbeHostActionPrefixSize && string(prefix[:len(hostActionDockerMagic)]) == hostActionDockerMagic
}

// DecodeProbeHostActionIntent combines exact kind bytes with an exact identity
// header (prefix plus JSON) into a valid host-action intent. docker must be
// the frame's version: a v1 frame carries only a systemd stage header and a
// v2 frame only a Docker header.
func DecodeProbeHostActionIntent(kind, header []byte, docker bool) (domain.ProbeHostActionIntent, error) {
	var zero domain.ProbeHostActionIntent
	if docker {
		return decodeProbeDockerActionIntent(kind, header)
	}
	stage, err := DecodeProbeBinaryStageHeader(header)
	if err != nil {
		return zero, domain.ErrProbeBinaryStageWriteInput
	}
	intent := domain.ProbeHostActionIntent{Identity: stage.Identity, OwnerID: stage.OwnerID, AttemptID: stage.AttemptID, Kind: string(kind)}
	if intent.Identity.Mode != domain.ProbeInstallSystemd || !intent.Valid() {
		return zero, domain.ErrProbeBinaryStageWriteInput
	}
	return intent, nil
}

// EncodeProbeHostActionGrantFrame writes the same fixed PHXGRT1 frame as a
// binary-stage grant; the signing domain, not the frame, separates them.
func EncodeProbeHostActionGrantFrame(grant domain.ProbeHostActionGrant) ([]byte, error) {
	if !grant.Request.Valid() || grant.Signature == ([64]byte{}) {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	frame := make([]byte, 0, ProbeBinaryStageGrantFrameSize)
	frame = append(frame, stageGrantMagic...)
	return append(frame, grant.Signature[:]...), nil
}

// EncodeProbeHostActionReceipt renders the one canonical success line.
func EncodeProbeHostActionReceipt(intent domain.ProbeHostActionIntent) []byte {
	marker := intent.MarkerSHA256()
	if marker == "" {
		return nil
	}
	line := make([]byte, 0, len(hostActionReceiptPrefix)+len(intent.Kind)+len(hostActionReceiptMiddle)+64+len(hostActionReceiptSuffix))
	line = append(line, hostActionReceiptPrefix...)
	line = append(line, intent.Kind...)
	line = append(line, hostActionReceiptMiddle...)
	line = append(line, marker...)
	return append(line, hostActionReceiptSuffix...)
}

// ProbeHostActionReceiptMatches accepts only the exact canonical line for
// this intent. A match is an acknowledgement shape, not an observation.
func ProbeHostActionReceiptMatches(line []byte, intent domain.ProbeHostActionIntent) bool {
	want := EncodeProbeHostActionReceipt(intent)
	return want != nil && string(line) == string(want)
}

func hostActionStageIntent(intent domain.ProbeHostActionIntent) domain.ProbeBinaryStageIntent {
	return domain.ProbeBinaryStageIntent{Identity: intent.Identity, OwnerID: intent.OwnerID, AttemptID: intent.AttemptID}
}
