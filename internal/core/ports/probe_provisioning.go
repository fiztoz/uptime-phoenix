package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeHostInspector verifies an explicitly trusted SSH host before sending any
// authentication, then runs only fixed nonmutating discovery commands. It must
// enforce destination policy on the exact dialed IP, bound all output and work,
// honor cancellation through handshake and execution, and return only sanitized
// errors/facts. No SSH credential is persisted or included in its result.
type ProbeHostInspector interface {
	InspectHost(ctx context.Context, host domain.ProbeSSHHost) (domain.ProbeHostFacts, error)
}
