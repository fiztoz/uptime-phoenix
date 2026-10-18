//go:build linux || darwin

package provisioning

import (
	"context"
	"os"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// OpenProbeImageLayerSource opens only a bounded regular file, nonblocking and
// read-only, through the existing artifact source policy.
func OpenProbeImageLayerSource(ctx context.Context, path string) (*os.File, error) {
	f, err := openProbeArtifactSource(ctx, path, domain.MaxProbeImageLayerBytes)
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, domain.ErrProbeImageInput
	}
	return f, nil
}
