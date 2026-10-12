package repository

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ReadAppliedRemote returns a point-in-time, receipt-backed configuration graph.
// It never reconstructs from mutable desired rows or selects the latest prepared
// snapshot. DesiredAlertDelivery is not proof of runtime delivery-mode application.
// A caller must separately fence any subsequent write or provider authorization.
func (s *RemoteProbeConfigSyncStore) ReadAppliedRemote(ctx context.Context, target domain.ProbeConfigTarget) (*domain.EdgeResolvedConfig, error) {
	if s == nil || s.db == nil || s.decoder == nil || s.protector == nil || !domain.ValidProbeConfigTarget(target) || !validRemoteProbeID(target.ProbeID) {
		return nil, domain.ErrValidation
	}

	snapshot, active, err := readAcknowledgedConfigTuple(ctx, s.db, target)
	if err != nil {
		return nil, remoteSyncError(ctx, err)
	}

	// Release the read transaction before decryption/extension validation. The
	// captured bytes are immutable; no shared lock spans validation or future I/O.
	metadata := snapshot.domain().ProbeConfigMetadata
	plain, err := s.protector.Open(ctx, metadata, snapshot.ProtectedPayload)
	if err != nil {
		return nil, remoteSyncError(ctx, err)
	}
	defer clear(plain)
	resolved, err := s.decoder.DecodeEdge(ctx, plain, target)
	if err != nil {
		return nil, remoteSyncError(ctx, err)
	}
	if resolved == nil || !domain.SameProbeConfigMetadata(resolved.Metadata, metadata) || len(resolved.Assignments) != active.AssignmentCount {
		return nil, ports.ErrConflict
	}
	return resolved, nil
}
