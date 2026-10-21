//go:build !linux && !darwin

package provisioning

import (
	"context"
	"os"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// NewLocalBinaryStageWriter refuses unsupported platforms before any mutation.
func NewLocalBinaryStageWriter(ctx context.Context, parent *os.File) (*LocalBinaryStageWriter, error) {
	if imageSnapshotNil(ctx) || parent == nil {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, domain.ErrProbeBinaryStageWriteUnavailable
}

func (w *LocalBinaryStageWriter) stageBinary(context.Context, domain.ProbeBinaryStageIntent, ports.PreparedProbeBinary) (domain.ProbeBinaryStageSeal, error) {
	// Field reads keep the shared struct used on this platform and do no I/O.
	if w != nil {
		_ = w.parent
		_ = w.afterChildFD
		_ = w.beforeReadback
		_ = w.beforeMarker
		_ = w.beforeDirCheck
		_ = w.syncFault
		_ = w.closeFault
		_ = w.writeChunk
	}
	return domain.ProbeBinaryStageSeal{}, domain.ErrProbeBinaryStageWriteUnavailable
}
