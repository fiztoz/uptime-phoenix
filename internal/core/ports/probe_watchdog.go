package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeWatchdogRepository owns a source checkpoint, lifecycle and delivery
// effects atomically. The hub and private edge adapters fence their own authority;
// mirror ingestion never invokes this port to create source work.
type ProbeWatchdogRepository interface {
	ReadWatchdog(ctx context.Context, authority domain.ProbeWatchdogAuthority) (domain.ProbeWatchdogState, error)
	CommitWatchdog(ctx context.Context, authority domain.ProbeWatchdogAuthority, record domain.ProbeWatchdogRecord) (domain.ProbeWatchdogState, error)
}

// ProbeHealthAdmission orders local receipt capture with watchdog ticks. The
// decoder must perform bounded in-memory validation only: no I/O, close, waiting
// or external callbacks. Nil health denotes a validated non-health frame. Errors
// leave admission unchanged and must close the session after releasing the gate.
type ProbeHealthAdmission interface {
	Admit(generation int64, decode func(time.Time) (*bool, error)) error
}
