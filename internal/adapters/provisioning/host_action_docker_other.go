//go:build !linux && !darwin

package provisioning

import (
	"context"
	"os"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeDocker runs one fixed-argument docker invocation from a fixed absolute
// path with a bounded stdout. Docker host actions are unavailable on this
// platform; the interface exists so callers compile.
type ProbeDocker interface {
	Run(ctx context.Context, args ...string) (stdout string, exitCode int, err error)
}

// ProbeImageStageDirName is the single directory, beneath the host's protected
// /var/lib, that image_cache payloads are staged in.
const ProbeImageStageDirName = "uptime-phoenix-image-stage"

// ApplyProbeImageCache is unavailable on this platform.
func ApplyProbeImageCache(context.Context, string, domain.ProbeHostActionIntent, ProbeDocker) error {
	return domain.ErrProbeBinaryStageWriteUnavailable
}

// ApplyProbeContainer is unavailable on this platform.
func ApplyProbeContainer(context.Context, domain.ProbeHostActionIntent, ProbeDocker) error {
	return domain.ErrProbeBinaryStageWriteUnavailable
}

// ApplyProbeDockerInit is unavailable on this platform.
func ApplyProbeDockerInit(context.Context, *os.File, domain.ProbeHostActionIntent, ProbeDocker) (ProbeInitResult, error) {
	return ProbeInitResult{}, domain.ErrProbeBinaryStageWriteUnavailable
}

// ApplyProbeDockerStart is unavailable on this platform.
func ApplyProbeDockerStart(context.Context, domain.ProbeHostActionIntent, ProbeDocker) error {
	return domain.ErrProbeBinaryStageWriteUnavailable
}

// ApplyProbeDockerCleanup is unavailable on this platform.
func ApplyProbeDockerCleanup(context.Context, *os.File, domain.ProbeHostActionIntent, ProbeDocker) error {
	return domain.ErrProbeBinaryStageWriteUnavailable
}

// ProbeImageStageFile is unavailable on this platform.
type ProbeImageStageFile struct{}

// OpenProbeImageStageFile is unavailable on this platform.
func OpenProbeImageStageFile(context.Context, *os.File, string, domain.ProbeHostActionIntent) (*ProbeImageStageFile, error) {
	return nil, domain.ErrProbeBinaryStageWriteUnavailable
}

// Write is unavailable on this platform.
func (*ProbeImageStageFile) Write([]byte) (int, error) { return 0, os.ErrClosed }

// Sync is unavailable on this platform.
func (*ProbeImageStageFile) Sync() error { return os.ErrClosed }

// Close is unavailable on this platform.
func (*ProbeImageStageFile) Close() error { return nil }

// Path is empty on this platform.
func (*ProbeImageStageFile) Path() string { return "" }

// Remove is unavailable on this platform.
func (*ProbeImageStageFile) Remove() error { return nil }
