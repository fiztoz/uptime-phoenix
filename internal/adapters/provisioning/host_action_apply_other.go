//go:build !linux && !darwin

package provisioning

import (
	"context"
	"os"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ApplyProbeDataDirectory is unavailable on this platform.
func ApplyProbeDataDirectory(context.Context, *os.File, domain.ProbeHostActionIntent) error {
	return domain.ErrProbeBinaryStageWriteUnavailable
}

// ApplyProbeUnitFile is unavailable on this platform.
func ApplyProbeUnitFile(context.Context, *os.File, domain.ProbeHostActionIntent) error {
	return domain.ErrProbeBinaryStageWriteUnavailable
}

// ApplyProbeService is unavailable on this platform.
func ApplyProbeService(context.Context, *os.File, domain.ProbeHostActionIntent, ProbeSystemctl) error {
	return domain.ErrProbeBinaryStageWriteUnavailable
}

// ApplyProbeInit is unavailable on this platform.
func ApplyProbeInit(context.Context, *os.File, *os.File, domain.ProbeHostActionIntent, ProbeCommandRunner) (ProbeInitResult, error) {
	return ProbeInitResult{}, domain.ErrProbeBinaryStageWriteUnavailable
}

// ApplyProbeServiceStart is unavailable on this platform.
func ApplyProbeServiceStart(context.Context, *os.File, domain.ProbeHostActionIntent, ProbeSystemctl) error {
	return domain.ErrProbeBinaryStageWriteUnavailable
}
