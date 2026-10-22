//go:build !linux

package bootstrap

import (
	"context"
	"os"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// openHostProbeBinaryStageParent never opens a target on unsupported platforms.
func openHostProbeBinaryStageParent(ctx context.Context) (*os.File, error) {
	if probeBinaryStageHelperNil(ctx) {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	if err := ctx.Err(); err != nil {
		return nil, probeBinaryStageHelperContextError(err)
	}
	return nil, domain.ErrProbeBinaryStageWriteUnavailable
}
