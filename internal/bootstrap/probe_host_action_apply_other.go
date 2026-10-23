//go:build !linux

package bootstrap

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// applyHostProbeAction never changes host state on unsupported platforms.
func applyHostProbeAction(ctx context.Context, _ domain.ProbeHostActionIntent) error {
	if probeBinaryStageHelperNil(ctx) {
		return domain.ErrProbeBinaryStageWriteInput
	}
	return domain.ErrProbeBinaryStageWriteUnavailable
}
