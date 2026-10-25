package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeBinaryStageInspector reads only an operation-derived remote marker/binary.
// It must join local I/O before returning. It may neither mutate remote state nor
// treat a matching observation as a journal grant, host fence or cleanup token.
type ProbeBinaryStageInspector interface {
	InspectBinaryStage(ctx context.Context, intent domain.ProbeBinaryStageIntent) (domain.ProbeBinaryStageObservation, error)
}
