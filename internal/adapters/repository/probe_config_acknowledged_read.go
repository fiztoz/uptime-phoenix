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

// readAcknowledgedConfigTuple captures one exact stored document without source
// reconstruction or write locks. Callers validate their local/remote target and
// authenticate/validate the captured bytes only after this transaction ends.
func readAcknowledgedConfigTuple(ctx context.Context, db *bun.DB, target domain.ProbeConfigTarget) (probeConfigModel, probeActiveConfigModel, error) {
	var snapshot probeConfigModel
	var active probeActiveConfigModel
	// Do not inherit READ COMMITTED: all SELECTs must observe one committed
	// pointer/receipt/snapshot tuple during concurrent application receipts.
	var options *sql.TxOptions
	if db.Dialect().Name() == dialect.MySQL {
		options = &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	}
	err := db.RunInTx(ctx, options, func(ctx context.Context, tx bun.Tx) error {
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
				return ports.ErrConflict // Never select another retained revision.
			}
			return err
		}
		if snapshot.HubID != active.HubID || snapshot.SHA256 != active.SHA256 {
			return ports.ErrConflict
		}
		return nil
	})
	return snapshot, active, err
}
