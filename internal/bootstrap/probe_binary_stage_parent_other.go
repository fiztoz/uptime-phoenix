//go:build !linux && !darwin

package bootstrap

import (
	"context"
	"io"
	"os"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func inheritedProbeBinaryStageParent(ctx context.Context) (*os.File, error) {
	if probeBinaryStageHelperNil(ctx) {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, domain.ErrProbeBinaryStageWriteUnavailable
}

func inheritedProbeBinaryStageInput(_ context.Context, input io.ReadCloser) (io.ReadCloser, error) {
	return input, nil // Parent acquisition already refuses unsupported platforms.
}
