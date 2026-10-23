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
	// ProbeHostActionPrefixSize is the PHXACT1 newline magic plus one kind
	// length byte. The kind bytes and an unchanged stage header follow.
	ProbeHostActionPrefixSize = 9
	// MaxProbeHostActionKindSize bounds the kind name.
	MaxProbeHostActionKindSize = 32

	hostActionMagic         = "PHXACT1\n"
	hostActionReceiptPrefix = `{"state":"host_action_applied","kind":"`
	hostActionReceiptMiddle = `","marker_sha256":"`
	hostActionReceiptSuffix = "\"}\n"
)

// EncodeProbeHostActionFrame writes the action prefix, the kind and the
// unchanged stage header of the intent's identity/owner/attempt. Invalid
// intents return nil.
func EncodeProbeHostActionFrame(intent domain.ProbeHostActionIntent) ([]byte, error) {
	if !intent.Valid() || len(intent.Kind) > MaxProbeHostActionKindSize {
		return nil, domain.ErrProbeBinaryStageWriteInput
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

// ProbeHostActionKindLength reads the exact 9-byte prefix and returns the
// kind length N (1..32). It does not inspect anything that follows.
func ProbeHostActionKindLength(prefix []byte) (int, error) {
	if len(prefix) != ProbeHostActionPrefixSize || string(prefix[:len(hostActionMagic)]) != hostActionMagic {
		return 0, domain.ErrProbeBinaryStageWriteInput
	}
	n := int(prefix[len(hostActionMagic)])
	if n < 1 || n > MaxProbeHostActionKindSize {
		return 0, domain.ErrProbeBinaryStageWriteInput
	}
	return n, nil
}

// DecodeProbeHostActionIntent combines exact kind bytes with an exact stage
// header (prefix plus JSON) into a valid host-action intent.
func DecodeProbeHostActionIntent(kind, header []byte) (domain.ProbeHostActionIntent, error) {
	var zero domain.ProbeHostActionIntent
	stage, err := DecodeProbeBinaryStageHeader(header)
	if err != nil {
		return zero, domain.ErrProbeBinaryStageWriteInput
	}
	intent := domain.ProbeHostActionIntent{Identity: stage.Identity, OwnerID: stage.OwnerID, AttemptID: stage.AttemptID, Kind: string(kind)}
	if !intent.Valid() {
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
