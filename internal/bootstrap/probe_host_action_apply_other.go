//go:build !linux

package bootstrap

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// applyHostProbeAction never changes host state on unsupported platforms.
func applyHostProbeAction(ctx context.Context, _ domain.ProbeHostActionIntent) ([]byte, error) {
	if probeBinaryStageHelperNil(ctx) {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	return nil, domain.ErrProbeBinaryStageWriteUnavailable
}
