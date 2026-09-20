package repository

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeWatchdogConfigReader reads the exact remotely applied graph for one
// fenced hub runtime. It never substitutes the latest prepared or mutable source.
type ProbeWatchdogConfigReader struct {
	store     *ProbeWatchdogStore
	authority domain.ProbeWatchdogAuthority
	decoder   ports.EdgeConfigDecoder
}

var _ ports.EdgeConfigReader = (*ProbeWatchdogConfigReader)(nil)

// NewProbeWatchdogConfigReader binds a cold reader to one runtime owner.
func NewProbeWatchdogConfigReader(store *ProbeWatchdogStore, authority domain.ProbeWatchdogAuthority, decoder ports.EdgeConfigDecoder) *ProbeWatchdogConfigReader {
	authority.HealthGeneration = 0
	return &ProbeWatchdogConfigReader{store: store, authority: authority, decoder: decoder}
}

// Load authenticates the applied pointer and retained exact snapshot. Later source
// commits must still atomically recheck config revision and ownership themselves.
func (r *ProbeWatchdogConfigReader) Load(ctx context.Context) (*domain.EdgeResolvedConfig, error) {
	if r == nil || r.store == nil || r.store.db == nil || r.store.protector == nil || r.decoder == nil || !validHubWatchdogAuthority(r.authority) {
		return nil, domain.ErrValidation
	}
	var snapshot *domain.ProtectedProbeConfig
	err := runConfigAuthorityTx(ctx, r.store.db, func(ctx context.Context, tx bun.Tx) error {
		if _, err := r.store.lockAuthority(ctx, tx, r.authority); err != nil {
			return err
		}
		var active probeActiveConfigModel
		if err := tx.NewSelect().Model(&active).Where("probe_id=?", r.authority.ProbeID).Scan(ctx); err != nil {
			return err
		}
		if active.HubID != r.authority.HubID {
			return ports.ErrConflict
		}
		var row probeConfigModel
		if err := tx.NewSelect().Model(&row).Where("probe_id=? AND revision=?", active.ProbeID, active.Revision).Scan(ctx); err != nil {
			return err
		}
		if row.HubID != active.HubID || row.SHA256 != active.SHA256 {
			return ports.ErrConflict
		}
		snapshot = row.domain()
		return nil
	})
	if err != nil {
		return nil, remoteSyncError(ctx, err)
	}
	plain, err := r.store.protector.Open(ctx, snapshot.ProbeConfigMetadata, snapshot.ProtectedPayload)
	if err != nil {
		return nil, remoteSyncError(ctx, err)
	}
	defer clear(plain)
	resolved, err := r.decoder.DecodeEdge(ctx, plain, snapshot.ProbeConfigTarget)
	if err != nil {
		return nil, remoteSyncError(ctx, err)
	}
	if resolved == nil || !domain.SameProbeConfigMetadata(resolved.Metadata, snapshot.ProbeConfigMetadata) {
		return nil, domain.ErrValidation
	}
	return resolved, nil
}
