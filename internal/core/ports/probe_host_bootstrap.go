package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeHostBootstrapper performs the one-time password-authenticated host
// preparation for the restricted-key M8 installer: it verifies the pinned host
// key before sending the password, installs the pinned helper, the hub's
// stage-authority public key and the restricted forced-command entry for the
// deployment key, then verifies that entry with the deployment key alone.
//
// It borrows the request's Password and PrivateKey buffers and must not retain
// them (or anything derived from the password) after returning. Failures are
// the context's error, domain.ErrProbeInstallArtifact for a rejected helper
// pin, domain.ErrProbeSSHInput for unparsable key/trust input, or a
// *domain.ProbeHostBootstrapError with one fixed code.
type ProbeHostBootstrapper interface {
	Bootstrap(ctx context.Context, req domain.ProbeHostBootstrapRequest) (domain.ProbeHostBootstrapResult, error)
}
