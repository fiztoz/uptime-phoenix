package repository

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// observationBatchWrites applies the current-state and dirty-bucket effects
// of one observation-only replay batch in a fixed number of statements. The
// batch holds the local registration's shared lock from its first statement
// to commit, so every round trip it saves shortens the wait of assignment
// replacement, which takes that row exclusively.
//
// Events are still decided one at a time in source order, by the same rules
// as updateCurrentProbeState, against rows read once up front and updated in
// memory. Only the final row per monitor is written, after the last event.
// Nothing else in an observation batch reads these tables.
type observationBatchWrites struct {
	dirty   []domain.DirtyBucket
	current map[probeMonitorKey]*observationCurrentRow
}

type probeMonitorKey struct {
	probeID   string
	monitorID int64
}

type observationCurrentRow struct {
	assignment *probeAssignmentModel
	missing    *probeMissingStateModel
	existing   *monitorProbeStateModel
	next       *domain.RegionalState
}

// loadObservationBatchWrites reads, per probe, the active assignment, missing
// marker and current state of every monitor the batch's new observations
// name. monitors holds the monitors whose assignment authority is locked.
func loadObservationBatchWrites(ctx context.Context, tx bun.Tx, events []domain.ProbeReplayEvent, committed int64, monitors map[int64]bool) (*observationBatchWrites, error) {
	w := &observationBatchWrites{current: make(map[probeMonitorKey]*observationCurrentRow)}
	byProbe := make(map[string][]int64)
	for _, event := range events {
		o := event.Observation
		if event.Seq <= committed || o == nil || !monitors[o.MonitorID] {
			continue
		}
		key := probeMonitorKey{probeID: o.ProbeID, monitorID: o.MonitorID}
		if w.current[key] == nil {
			w.current[key] = &observationCurrentRow{}
			byProbe[o.ProbeID] = append(byProbe[o.ProbeID], o.MonitorID)
		}
	}
	for probeID, ids := range byProbe {
		slices.Sort(ids)
		var assignments []probeAssignmentModel
		if err := tx.NewSelect().Model(&assignments).Where("probe_id = ? AND active = ? AND monitor_id IN (?)", probeID, true, bun.List(ids)).Order("monitor_id ASC").Scan(ctx); err != nil {
			return nil, err
		}
		for i := range assignments {
			w.current[probeMonitorKey{probeID, assignments[i].MonitorID}].assignment = &assignments[i]
		}
		var missing []probeMissingStateModel
		if err := tx.NewSelect().Model(&missing).Where("probe_id = ? AND monitor_id IN (?)", probeID, bun.List(ids)).Order("monitor_id ASC").Scan(ctx); err != nil {
			return nil, err
		}
		for i := range missing {
			w.current[probeMonitorKey{probeID, missing[i].MonitorID}].missing = &missing[i]
		}
		var states []monitorProbeStateModel
		if err := tx.NewSelect().Model(&states).Where("probe_id = ? AND monitor_id IN (?)", probeID, bun.List(ids)).Order("monitor_id ASC").Scan(ctx); err != nil {
			return nil, err
		}
		for i := range states {
			w.current[probeMonitorKey{probeID, states[i].MonitorID}].existing = &states[i]
		}
	}
	return w, nil
}

// observe records one accepted observation: its history range now, its dirty
// buckets and current state at flush.
func (w *observationBatchWrites) observe(ctx context.Context, tx bun.Tx, obs domain.RegionalObservation, now time.Time) error {
	if err := markObservationHistoryRangeTx(ctx, tx, obs); err != nil {
		return err
	}
	w.dirty = append(w.dirty, domain.DirtyBucketsForObservation(obs)...)
	if err := syncRemoteConditionMeasurements(ctx, tx, obs); err != nil {
		return err
	}
	state := replayObservationState(obs, now)
	row := w.current[probeMonitorKey{probeID: state.ProbeID, monitorID: state.MonitorID}]
	if state.ObservedAt.After(now) || row == nil || row.assignment == nil || row.assignment.Generation != state.AssignmentGeneration || missingSuppresses(row.missing, state) {
		return nil
	}
	next, apply, _, err := nextCurrentProbeState(row.missing, row.existing, state, false)
	if err != nil || !apply {
		return err
	}
	// What the per-event path's writes leave behind for the next event.
	written := regionalStateRow(next)
	row.missing, row.existing, row.next = nil, &written, &next
	return nil
}

// flush writes the batch's final current states, TLS evidence and dirty
// buckets, each in monitor order.
func (w *observationBatchWrites) flush(ctx context.Context, tx bun.Tx) error {
	keys := make([]probeMonitorKey, 0, len(w.current))
	for key, row := range w.current {
		if row.next != nil {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b probeMonitorKey) int {
		return cmp.Or(strings.Compare(a.probeID, b.probeID), cmp.Compare(a.monitorID, b.monitorID))
	})
	for start := 0; start < len(keys); {
		end := start
		for end < len(keys) && keys[end].probeID == keys[start].probeID {
			end++
		}
		if err := w.flushProbe(ctx, tx, keys[start:end]); err != nil {
			return err
		}
		start = end
	}
	return markDirtyTx(ctx, tx, w.dirty)
}

func (w *observationBatchWrites) flushProbe(ctx context.Context, tx bun.Tx, keys []probeMonitorKey) error {
	probeID := keys[0].probeID
	ids := make([]int64, 0, len(keys))
	states := make([]monitorProbeStateModel, 0, len(keys))
	var tls []TLSInfoModel
	var cleared []any
	for _, key := range keys {
		next := w.current[key].next
		ids = append(ids, key.monitorID)
		states = append(states, regionalStateRow(*next))
		if probeID == domain.LocalProbeID {
			continue
		}
		if next.TLS == nil {
			cleared = append(cleared, next.MonitorID, next.AssignmentGeneration)
		} else {
			tls = append(tls, remoteTLSInfoRow(*next))
		}
	}
	if _, err := tx.NewDelete().Model((*probeMissingStateModel)(nil)).Where("probe_id = ? AND monitor_id IN (?)", probeID, bun.List(ids)).Exec(ctx); err != nil {
		return err
	}
	q := tx.NewInsert().Model(&states)
	columns := []string{"tls_json", "conditions_json", "assignment_generation", "stream_id", "seq", "config_revision", "status", "down_count", "observed_at", "received_at", "last_success_at", "ping", "message", "active_source_alert_id"}
	if tx.Dialect().Name() == dialect.MySQL {
		q = q.On("DUPLICATE KEY UPDATE")
		for _, c := range columns {
			q = q.Set("? = VALUES(?)", bun.Ident(c), bun.Ident(c))
		}
	} else {
		q = q.On("CONFLICT(monitor_id,probe_id) DO UPDATE")
		for _, c := range columns {
			q = q.Set("? = EXCLUDED.?", bun.Ident(c), bun.Ident(c))
		}
	}
	if _, err := q.Exec(ctx); err != nil {
		return err
	}
	if len(cleared) > 0 {
		pairs := strings.TrimSuffix(strings.Repeat("(?, ?), ", len(cleared)/2), ", ")
		if _, err := tx.NewDelete().Model((*TLSInfoModel)(nil)).Where("probe_id = ?", probeID).Where("(monitor_id, assignment_generation) IN ("+pairs+")", cleared...).Exec(ctx); err != nil {
			return err
		}
	}
	if len(tls) > 0 {
		q := tx.NewInsert().Model(&tls)
		if tx.Dialect().Name() == dialect.MySQL {
			q = q.On("DUPLICATE KEY UPDATE").Set("info_json = VALUES(info_json)").Set("checked_at = VALUES(checked_at)")
		} else {
			q = q.On("CONFLICT (monitor_id, probe_id, assignment_generation) DO UPDATE").Set("info_json = EXCLUDED.info_json").Set("checked_at = EXCLUDED.checked_at")
		}
		if _, err := q.Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}
