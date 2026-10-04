package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// RemoteProbeConfigEncoder maps a resolved source graph to the remote wire
// dialect. Its input shares the existing definition type with the local encoder.
type RemoteProbeConfigEncoder interface {
	EncodeRemote(definition domain.LocalProbeConfigDefinition) ([]byte, error)
}

// RemoteProbeConfigSyncRepository publishes complete desired snapshots from
// authorized saved source, and records exact application receipts under a lease.
// Latest prepared revision versus active revision is the durable pending work.
type RemoteProbeConfigSyncRepository interface {
	RefreshRemote(ctx context.Context, target domain.ProbeConfigTarget, at time.Time) (domain.ProbeConfigMetadata, error)
	RecordRemoteApplied(ctx context.Context, lease domain.ProbeConnectorLease, receipt domain.ProbeActiveConfig) error
}
