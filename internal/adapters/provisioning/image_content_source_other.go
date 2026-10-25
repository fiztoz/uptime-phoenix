//go:build !linux && !darwin

package provisioning

import (
	"context"
	"os"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// OpenProbeImageLayerSource refuses platforms without the regular-file policy.
func OpenProbeImageLayerSource(ctx context.Context, _ string) (*os.File, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, domain.ErrProbeImageInput
}
