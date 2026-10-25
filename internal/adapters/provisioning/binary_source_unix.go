//go:build linux || darwin

package provisioning

import (
	"context"
	"os"
	"syscall"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// OpenProbeBinarySource opens read-only without blocking on FIFOs/devices.
// Symlinks are permitted only when their opened target is a regular file.
func OpenProbeBinarySource(ctx context.Context, path string) (*os.File, error) {
	return openProbeArtifactSource(ctx, path, domain.MaxProbeBinarySize)
}

// OpenProbeImageMetadataSource applies the same regular-file policy to bounded metadata.
func OpenProbeImageMetadataSource(ctx context.Context, path string) (*os.File, error) {
	f, err := openProbeArtifactSource(ctx, path, domain.MaxProbeImageMetadataSize)
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, domain.ErrProbeImageInput
	}
	return f, nil
}

func openProbeArtifactSource(ctx context.Context, path string, maxSize int64) (*os.File, error) {
	if ctx == nil || path == "" {
		return nil, domain.ErrProbeArtifactInput
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, domain.ErrProbeArtifactInput
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxSize {
		_ = f.Close()
		return nil, domain.ErrProbeArtifactInput
	}
	if ctx.Err() != nil {
		_ = f.Close()
		return nil, ctx.Err()
	}
	return f, nil
}
