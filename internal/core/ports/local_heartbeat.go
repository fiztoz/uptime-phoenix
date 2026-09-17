package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// LocalHeartbeatRecorder allocates a stream-wide sequence and atomically writes
// the local heartbeat, regional observation/state, and dirty buckets. A failed
// commit consumes no sequence and returns no heartbeat. ErrStaleLocalState means
// the caller must re-evaluate against newer state before retrying. Notification
// I/O and auxiliary state are outside this transaction.
type LocalHeartbeatRecorder interface {
	GetState(ctx context.Context, monitorID int64, probeID string) (*domain.RegionalState, error)
	CommitLocalHeartbeat(ctx context.Context, commit domain.LocalHeartbeatCommit) (*domain.Heartbeat, error)
}
