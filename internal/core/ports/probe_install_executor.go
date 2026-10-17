package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeInstallResourceExecutor applies one newly acknowledged resource intent.
// The snapshot binds the exact identity/owner/resource/attempt; it is not a
// transferable authorization token, remote fence, verified artifact or cleanup
// authority. Implementations must independently enforce actual host trust,
// destination policy, artifact integrity, remote isolation/fencing and bounds.
// ApplyResource returns only after all work for that invocation has joined;
// closing a local SSH channel is not proof of remote process termination.
// Only joined valid reports with nil error are definite outcomes. not_started
// means no mutation was invoked; unknown is mandatory for ambiguous effects.
// Credentials, arbitrary commands and raw remote output never cross this port.
// The caller owns executor/session/artifact lifetime outside each invocation.
// There is intentionally no concrete remote adapter or installation-success API.
type ProbeInstallResourceExecutor interface {
	ApplyResource(ctx context.Context, operation domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error)
}
