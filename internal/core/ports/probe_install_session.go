package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeInstallSessionFactory parses one transient deployment key, verifies the
// pinned artifact from the operator's artifact directory, and returns a
// one-operation executor plus the key's PUBLIC fingerprint. It creates nothing
// durable and performs no SSH I/O. Artifact failures return
// domain.ErrProbeInstallArtifact; key/trust input failures return
// domain.ErrProbeSSHInput.
type ProbeInstallSessionFactory interface {
	NewInstallSession(ctx context.Context, privateKey []byte, hostFingerprint string, pin domain.ProbeBinaryPin) (ProbeInstallSession, string, error)
	// NewDockerInstallSession is the Docker counterpart: it verifies the
	// pinned image from the operator's OCI layout, packs the closed archive
	// image_cache streams, and also returns the verified image selection
	// (manifest, config, layer count) the install identity records.
	NewDockerInstallSession(ctx context.Context, privateKey []byte, hostFingerprint string, pin domain.ProbeImagePin) (ProbeInstallSession, string, domain.ProbeImageSelection, error)
}

// ProbeInstallSession executes one operation's resources and then must be
// closed, which disposes its prepared artifact snapshot. Close is idempotent.
// Preflight runs the host's fixed nonmutating install preflight with the
// session's key and trust; it changes nothing and returns the context's error
// or a *domain.ProbeInstallPreflightError on failure.
type ProbeInstallSession interface {
	ProbeInstallResourceExecutor
	Preflight(ctx context.Context, host domain.ProbeSSHHost) (domain.ProbeInstallHostReport, error)
	Close(ctx context.Context) error
}
