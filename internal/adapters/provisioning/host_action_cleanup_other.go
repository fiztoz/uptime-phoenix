//go:build !linux && !darwin

package provisioning

import (
	"context"
	"os"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ApplyProbeCleanup is unavailable on this platform.
func ApplyProbeCleanup(context.Context, *os.File, *os.File, *os.File, domain.ProbeHostActionIntent, ProbeSystemctl) error {
	return domain.ErrProbeBinaryStageWriteUnavailable
}
