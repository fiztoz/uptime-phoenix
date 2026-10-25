//go:build !linux

package bootstrap

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// gatherHostProbeInstallReport never qualifies an unsupported platform.
func gatherHostProbeInstallReport(context.Context) (domain.ProbeInstallHostReport, error) {
	return domain.ProbeInstallHostReport{}, domain.ErrProbeBinaryStageWriteUnavailable
}
