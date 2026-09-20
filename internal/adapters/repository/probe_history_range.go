package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type probeHistoryRangeModel struct {
	bun.BaseModel   `bun:"table:probe_history_ranges,alias:repair"`
	MonitorID       int64     `bun:"monitor_id,pk"`
	ProbeID         string    `bun:"probe_id,pk"`
	CursorAt        time.Time `bun:"cursor_at"`
	ObservedThrough time.Time `bun:"observed_through"`
}

// A later sequence with an earlier wall clock invalidates already projected
// future samples. Persist the whole affected range with the source commit;
// marking only the new sample's freshness window would miss those old buckets.
func markObservationHistoryTx(ctx context.Context, tx bun.Tx, obs domain.RegionalObservation) error {
	if obs.StreamID != "" && obs.Seq > 0 {
		var through time.Time
		err := tx.NewSelect().Model((*probeObservationModel)(nil)).Column("observed_at").
			Where("monitor_id = ? AND probe_id = ? AND assignment_generation = ? AND stream_id = ?", obs.MonitorID, obs.ProbeID, obs.AssignmentGeneration, obs.StreamID).
			Where("seq < ? AND observed_at > ?", obs.Seq, obs.ObservedAt.UTC()).Order("observed_at DESC", "id DESC").Limit(1).Scan(ctx, &through)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			row := probeHistoryRangeModel{MonitorID: obs.MonitorID, ProbeID: obs.ProbeID, CursorAt: obs.ObservedAt.UTC().Truncate(time.Minute), ObservedThrough: through.UTC()}
			q := tx.NewInsert().Model(&row)
			if tx.Dialect().Name() == dialect.MySQL {
				q = q.On("DUPLICATE KEY UPDATE").Set("cursor_at = LEAST(cursor_at, VALUES(cursor_at))").Set("observed_through = GREATEST(observed_through, VALUES(observed_through))")
			} else {
				q = q.On("CONFLICT(monitor_id,probe_id) DO UPDATE").Set("cursor_at = MIN(cursor_at, EXCLUDED.cursor_at)").Set("observed_through = MAX(observed_through, EXCLUDED.observed_through)")
			}
			if _, err := q.Exec(ctx); err != nil {
				return err
			}
		}
	}
	return markDirtyTx(ctx, tx, domain.DirtyBucketsForObservation(obs))
}

func (r *RegionalCommitStore) expandHistoryRange(ctx context.Context, now time.Time, projector ports.ProbeHistoryProjector) (bool, error) {
	changed := false
	err := r.db.RunInTx(ctx, historyTxOptions(r.db), func(ctx context.Context, tx bun.Tx) error {
		if tx.Dialect().Name() != dialect.MySQL {
			if _, err := tx.ExecContext(ctx, "UPDATE probe_history_ranges SET cursor_at=cursor_at WHERE monitor_id=(SELECT monitor_id FROM probe_history_ranges WHERE cursor_at < ? ORDER BY cursor_at,monitor_id,probe_id LIMIT 1)", now.Truncate(time.Minute)); err != nil {
				return err
			}
		}
		var row probeHistoryRangeModel
		q := tx.NewSelect().Model(&row).Where("cursor_at < ?", now.Truncate(time.Minute)).Order("cursor_at ASC", "monitor_id ASC", "probe_id ASC").Limit(1)
		if tx.Dialect().Name() == dialect.MySQL {
			q = q.For("UPDATE")
		}
		if err := q.Scan(ctx); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		var monitor MonitorModel
		if err := tx.NewSelect().Model(&monitor).Where("id = ?", row.MonitorID).Scan(ctx); err != nil {
			return err
		}
		fresh, err := projector.HistoryFreshness(*monitor.ToDomain())
		if err != nil {
			return err
		}
		if err := markDirtyTx(ctx, tx, domain.DirtyBucketsForObservation(domain.RegionalObservation{MonitorID: row.MonitorID, ProbeID: row.ProbeID, ObservedAt: row.CursorAt.UTC()})); err != nil {
			return err
		}
		row.CursorAt = row.CursorAt.UTC().Add(time.Minute)
		if row.CursorAt.After(row.ObservedThrough.UTC().Add(fresh).Truncate(time.Minute)) {
			_, err = tx.NewDelete().Model(&row).WherePK().Exec(ctx)
		} else {
			_, err = tx.NewUpdate().Model(&row).Column("cursor_at").WherePK().Exec(ctx)
		}
		changed = err == nil
		return err
	})
	return changed, err
}

func (r *RegionalCommitStore) expandHistoryRanges(ctx context.Context, now time.Time, projector ports.ProbeHistoryProjector) (bool, error) {
	// Give both kinds of range progress even while either has a long backlog.
	repaired, err := r.expandHistoryRange(ctx, now, projector)
	if err != nil {
		return false, err
	}
	gap, err := r.expandHistoryGap(ctx, now, projector)
	return repaired || gap, err
}
