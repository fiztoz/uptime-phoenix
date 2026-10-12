package repository

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// LocalProbeDeliveryConfigStore reads exact acknowledged local documents. It
// never invokes execution-source reconstruction, activation or provider I/O.
type LocalProbeDeliveryConfigStore struct {
	db        *bun.DB
	decoder   ports.LocalProbeDeliveryConfigDecoder
	protector ports.ProbeConfigProtector
}

var _ ports.LocalProbeDeliveryConfigReader = (*LocalProbeDeliveryConfigStore)(nil)

// NewLocalProbeDeliveryConfigStore binds the existing local decoder/protector.
func NewLocalProbeDeliveryConfigStore(db *bun.DB, decoder ports.LocalProbeDeliveryConfigDecoder, protector ports.ProbeConfigProtector) *LocalProbeDeliveryConfigStore {
	return &LocalProbeDeliveryConfigStore{db: db, decoder: decoder, protector: protector}
}

// ReadAcknowledgedLocalDelivery projects a read-only stored tuple after trusted
// target, authenticated metadata and local semantic validation. Facts are not
// execution, lease, mode-application or send/suppression authority.
func (s *LocalProbeDeliveryConfigStore) ReadAcknowledgedLocalDelivery(ctx context.Context, target domain.ProbeConfigTarget) (*domain.AcknowledgedProbeDeliveryConfig, error) {
	if s == nil || s.db == nil || s.decoder == nil || s.protector == nil || !domain.ValidProbeConfigTarget(target) || target.ProbeID != domain.LocalProbeID {
		return nil, domain.ErrValidation
	}
	snapshot, active, err := readAcknowledgedConfigTuple(ctx, s.db, target)
	if err != nil {
		return nil, remoteSyncError(ctx, err)
	}
	metadata := snapshot.domain().ProbeConfigMetadata
	plain, err := s.protector.Open(ctx, metadata, snapshot.ProtectedPayload)
	if err != nil {
		return nil, remoteSyncError(ctx, err)
	}
	defer clear(plain)
	result, err := s.decoder.DecodeLocalDelivery(ctx, plain, metadata)
	if err != nil {
		return nil, remoteSyncError(ctx, err)
	}
	if result == nil || result.Target != target || result.Revision != metadata.Revision || len(result.Assignments) != active.AssignmentCount {
		return nil, ports.ErrConflict
	}
	seen := make(map[int64]bool, len(result.Assignments))
	for _, a := range result.Assignments {
		if a.MonitorID <= 0 || a.Generation <= 0 || a.AssignmentRevision < 0 || !domain.ValidAlertDelivery(a.AlertDelivery) || seen[a.MonitorID] {
			return nil, ports.ErrConflict
		}
		seen[a.MonitorID] = true
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
