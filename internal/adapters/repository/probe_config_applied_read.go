package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

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

	var snapshot probeConfigModel
	var active probeActiveConfigModel
	// Explicit isolation is necessary: a MariaDB session may default to READ
	// COMMITTED. These three reads must select one committed pointer/receipt/
	// snapshot tuple even while another connector records a newer receipt.
	var options *sql.TxOptions
	if s.db.Dialect().Name() == dialect.MySQL {
		options = &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	}
	err := s.db.RunInTx(ctx, options, func(ctx context.Context, tx bun.Tx) error {
		if err := tx.NewSelect().Model(&active).Where("probe_id = ?", target.ProbeID).Scan(ctx); err != nil {
			return err
		}
		if active.HubID != target.HubID || !domain.ValidProbeActiveConfig(domain.ProbeActiveConfig{
			ProbeConfigTarget: target, Revision: active.Revision, SHA256: active.SHA256,
			AppliedAt: active.AppliedAt, AssignmentCount: active.AssignmentCount,
		}) {
			return ports.ErrConflict
		}
		var receipt probeConfigAppliedReceiptModel
		if err := tx.NewSelect().Model(&receipt).Where("probe_id = ? AND revision = ?", target.ProbeID, active.Revision).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ports.ErrConflict // A pointer alone is not a durable receipt.
			}
			return err
		}
		if receipt.HubID != active.HubID || receipt.SHA256 != active.SHA256 || receipt.AssignmentCount != active.AssignmentCount || !receipt.AppliedAt.Equal(active.AppliedAt) {
			return ports.ErrConflict
		}
		if err := tx.NewSelect().Model(&snapshot).Where("probe_id = ? AND revision = ?", target.ProbeID, active.Revision).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ports.ErrConflict // Never fall back to another retained document.
			}
			return err
		}
		if snapshot.HubID != active.HubID || snapshot.SHA256 != active.SHA256 {
			return ports.ErrConflict
		}
		return nil
	})
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
