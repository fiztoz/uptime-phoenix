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

	// ReadAppliedRemote authenticates and decodes the exact retained document
	// selected by a matching active pointer and durable configuration receipt.
	// DesiredAlertDelivery remains desired, not an applied-mode receipt. The
	// read grants neither lease authority nor permission to send or suppress.
	ReadAppliedRemote(ctx context.Context, target domain.ProbeConfigTarget) (*domain.EdgeResolvedConfig, error)
}

// RemoteRegionalApplicationRepository records authenticated effective regional
// facts after independently verifying the committed configuration receipt and
// retained document. It grants no cutover, send or suppression authority.
type RemoteRegionalApplicationRepository interface {
	RecordRemoteRegionalApplications(ctx context.Context, session domain.ProbeReplaySession, applications []domain.ProbeDeliveryModeApplication) error
}
