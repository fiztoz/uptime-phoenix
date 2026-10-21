package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeBinaryStageWriter borrows binary; only streams returned by Open are owned.
// It creates a fresh local stage only, never a journal resource outcome or fence.
// Once directory creation is attempted, all errors other than exclusive-name
// conflict are outcome-unknown; partial effects are retained and never repaired.
type ProbeBinaryStageWriter interface {
	StageBinary(ctx context.Context, intent domain.ProbeBinaryStageIntent, binary PreparedProbeBinary) (domain.ProbeBinaryStageSeal, error)
}
