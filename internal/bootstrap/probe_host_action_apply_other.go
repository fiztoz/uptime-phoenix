//go:build !linux

package bootstrap

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// applyHostProbeAction never changes host state on unsupported platforms.
func applyHostProbeAction(ctx context.Context, _ domain.ProbeHostActionIntent, _ string) ([]byte, error) {
	if probeBinaryStageHelperNil(ctx) {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	return nil, domain.ErrProbeBinaryStageWriteUnavailable
}

// stageHostProbeImage never stages anything on unsupported platforms.
func stageHostProbeImage(ctx context.Context, _ domain.ProbeHostActionIntent) (probeStagedArchive, error) {
	if probeBinaryStageHelperNil(ctx) {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	return nil, domain.ErrProbeBinaryStageWriteUnavailable
}
