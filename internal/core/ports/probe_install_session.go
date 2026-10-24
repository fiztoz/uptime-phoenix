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
}

// ProbeInstallSession executes one operation's resources and then must be
// closed, which disposes its prepared artifact snapshot. Close is idempotent.
type ProbeInstallSession interface {
	ProbeInstallResourceExecutor
	Close(ctx context.Context) error
}
