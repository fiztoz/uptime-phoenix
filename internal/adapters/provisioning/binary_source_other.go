//go:build !linux && !darwin

package provisioning

import (
	"context"
	"os"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// OpenProbeBinarySource refuses unsupported local operator platforms.
func OpenProbeBinarySource(ctx context.Context, path string) (*os.File, error) {
	return nil, domain.ErrProbeArtifactInput
}
