package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type historyClearWatermarkModel struct {
	bun.BaseModel        `bun:"table:history_clear_watermarks"`
	MonitorID            int64     `bun:"monitor_id,pk"`
	ProbeID              string    `bun:"probe_id,pk"`
	AssignmentGeneration int64     `bun:"assignment_generation,pk"`
	ClearID              string    `bun:"clear_id"`
	ThroughSeq           int64     `bun:"through_seq"`
	ThroughObservedAt    time.Time `bun:"through_observed_at"`
	ClearedAt            time.Time `bun:"cleared_at"`
	DroppedCount         int64     `bun:"dropped_count"`
}

func (m historyClearWatermarkModel) domain() domain.HistoryClearWatermark {
	return domain.HistoryClearWatermark{
		ClearID: m.ClearID, MonitorID: m.MonitorID, ProbeID: m.ProbeID,
		AssignmentGeneration: m.AssignmentGeneration, ThroughSeq: m.ThroughSeq,
		ThroughObservedAt: m.ThroughObservedAt.UTC(), ClearedAt: m.ClearedAt.UTC(),
		DroppedCount: m.DroppedCount,
	}
}

// HistoryClearStore implements the authorized clear-history action on either
// hub engine. It is dialect-neutral Bun like the probe registry store; the
// concrete database adapters expose it through their composition roots.
type HistoryClearStore struct{ db *bun.DB }

var _ ports.HistoryClearStore = (*HistoryClearStore)(nil)

// NewHistoryClearStore creates a Bun-backed clear-history store.
func NewHistoryClearStore(db *bun.DB) *HistoryClearStore { return &HistoryClearStore{db: db} }

// ClearMonitorHistory removes the monitor's history evidence and upserts one
// fence per active remote assignment in a single transaction. The bounds only
// ever widen, so a repeated clear (or a backward hub clock) can never reopen
// the fence. Unknown monitors are ErrNotFound; nothing is deleted for them.
func (r *HistoryClearStore) ClearMonitorHistory(ctx context.Context, monitorID int64, at time.Time) ([]domain.HistoryClearWatermark, error) {
	if monitorID < 1 || at.IsZero() {
		return nil, fmt.Errorf("clear monitor history: %w", domain.ErrValidation)
	}
	at = at.UTC().Truncate(time.Microsecond)
	out := make([]domain.HistoryClearWatermark, 0)
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		out = out[:0]
		// Lock order: monitor row first (same as monitor creation), then the
		// probe registrations in id order (same as assignment replacement and
		// configuration publication).
		if _, err := tx.NewUpdate().Table("monitors").Set("id = id").Where("id = ?", monitorID).Exec(ctx); err != nil {
			return err
		}
		exists, err := tx.NewSelect().Table("monitors").Where("id = ?", monitorID).Exists(ctx)
		if err != nil {
			return err
		}
		if !exists {
			return ports.ErrNotFound
		}
		type memberRow struct {
			ProbeID    string
			Generation int64
		}
		var members []memberRow
		if err := tx.NewSelect().Table("monitor_probe_assignments").
			Column("probe_id", "generation").
			Where("monitor_id = ? AND active = ? AND probe_id <> ?", monitorID, true, domain.LocalProbeID).
			Order("probe_id ASC").Scan(ctx, &members); err != nil {
			return err
		}
		for _, member := range members {
			if _, err := tx.NewUpdate().Table("probes").Set("revision = revision").Where("id = ?", member.ProbeID).Exec(ctx); err != nil {
				return err
			}
		}
		clearID := uuid.NewString()
		for _, member := range members {
			var throughSeq int64
			if err := tx.NewSelect().Table("probe_observations").
				ColumnExpr("COALESCE(MAX(seq), 0)").
				Where("monitor_id = ? AND probe_id = ?", monitorID, member.ProbeID).
				Scan(ctx, &throughSeq); err != nil {
				return err
			}
			wm := historyClearWatermarkModel{
				MonitorID: monitorID, ProbeID: member.ProbeID, AssignmentGeneration: member.Generation,
				ClearID: clearID, ThroughSeq: throughSeq, ThroughObservedAt: at, ClearedAt: at,
			}
			existing := new(historyClearWatermarkModel)
			err := tx.NewSelect().Model(existing).
				Where("monitor_id = ? AND probe_id = ? AND assignment_generation = ?", monitorID, member.ProbeID, member.Generation).
				Scan(ctx)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				if _, err := tx.NewInsert().Model(&wm).Exec(ctx); err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				// A repeated clear widens the fence and keeps counting drops.
				if existing.ThroughSeq > wm.ThroughSeq {
					wm.ThroughSeq = existing.ThroughSeq
				}
				if existing.ThroughObservedAt.After(wm.ThroughObservedAt) {
					wm.ThroughObservedAt = existing.ThroughObservedAt
				}
				wm.DroppedCount = existing.DroppedCount
				if _, err := tx.NewUpdate().Model(&wm).WherePK().Exec(ctx); err != nil {
					return err
				}
			}
			out = append(out, wm.domain())
		}
		// Remove the history evidence itself. Current state
		// (monitor_health_state / monitor_probe_state), incidents, delivery
		// outcomes, assignment rows and telemetry receipts are not history and
		// are deliberately untouched.
		for _, table := range []string{
			"probe_observations", "monitor_health_history", "probe_dirty_buckets",
			"heartbeat_1d", "heartbeat_1h", "heartbeat_1m", "heartbeats",
		} {
			if _, err := tx.NewDelete().Table(table).Where("monitor_id = ?", monitorID).Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("clear monitor history: %w", probeRegistryError(err))
	}
	return out, nil
}

// historyClearCovering returns the fence that covers one remote observation,
// or nil when the evidence is not cleared. Local execution has no replay path
// and installs no fence.
func historyClearCovering(ctx context.Context, tx bun.Tx, obs domain.RegionalObservation) (*historyClearWatermarkModel, error) {
	if obs.ProbeID == "" || obs.ProbeID == domain.LocalProbeID {
		return nil, nil
	}
	row := new(historyClearWatermarkModel)
	err := tx.NewSelect().Model(row).
		Where("monitor_id = ? AND probe_id = ? AND assignment_generation = ?",
			obs.MonitorID, obs.ProbeID, obs.AssignmentGeneration).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if row.domain().Cleared(obs.Seq, obs.ObservedAt) {
		return row, nil
	}
	return nil, nil
}

// countHistoryClearDrop acknowledges one deliberate drop on its fence.
func countHistoryClearDrop(ctx context.Context, tx bun.Tx, fence historyClearWatermarkModel) error {
	_, err := tx.NewUpdate().Table("history_clear_watermarks").
		Set("dropped_count = dropped_count + 1").
		Where("monitor_id = ? AND probe_id = ? AND assignment_generation = ?",
			fence.MonitorID, fence.ProbeID, fence.AssignmentGeneration).
		Exec(ctx)
	return err
}
