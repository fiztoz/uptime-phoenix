package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeWatchdogRepository owns a source checkpoint, lifecycle and delivery
// effects atomically. The hub and private edge adapters fence their own authority;
// mirror ingestion never invokes this port to create source work.
type ProbeWatchdogRepository interface {
	ReadWatchdog(ctx context.Context, authority domain.ProbeWatchdogAuthority) (domain.ProbeWatchdogState, error)
	CommitWatchdog(ctx context.Context, authority domain.ProbeWatchdogAuthority, record domain.ProbeWatchdogRecord) (domain.ProbeWatchdogState, error)
}
