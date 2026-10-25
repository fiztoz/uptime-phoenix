//go:build !linux

package bootstrap

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// loadHostProbeBinaryStageAuthority never trusts a key on unsupported platforms.
func loadHostProbeBinaryStageAuthority(ctx context.Context) ([32]byte, error) {
	if probeBinaryStageHelperNil(ctx) {
		return [32]byte{}, domain.ErrProbeBinaryStageWriteInput
	}
	if err := ctx.Err(); err != nil {
		return [32]byte{}, probeBinaryStageHelperContextError(err)
	}
	return [32]byte{}, domain.ErrProbeBinaryStageWriteUnavailable
}

// loadHostProbeBinaryStageHostKeys never binds an intent on unsupported platforms.
func loadHostProbeBinaryStageHostKeys(ctx context.Context) ([]string, error) {
	if probeBinaryStageHelperNil(ctx) {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	if err := ctx.Err(); err != nil {
		return nil, probeBinaryStageHelperContextError(err)
	}
	return nil, domain.ErrProbeBinaryStageWriteUnavailable
}
