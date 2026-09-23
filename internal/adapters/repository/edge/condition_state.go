package edge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// edgeConditionRow is the durable, source-owned evaluated auxiliary condition
// for one immutable assignment generation and condition kind. Times follow the
// edge microsecond storage contract.
type edgeConditionRow struct {
	bun.BaseModel    `bun:"table:edge_condition_state"`
	MonitorID        int64  `bun:",pk"`
	Generation       int64  `bun:",pk"`
	Kind             string `bun:",pk"`
	ConfigRevision   int64
	ObservedState    string
	EffectiveState   *string
	ConsecutiveState string
	ConsecutiveCount int
	UsedValue        *float64
	LimitValue       *float64
	PercentValue     *float64
	ThresholdValue   *float64
	Unit             string
	Resource         string
	Scope            string
	Source           string
	Message          string
	ObservedAt       int64
	StaleAfter       int64
	LastSuccessAt    *int64
	Version          int64
}

func (row edgeConditionRow) state() (domain.EdgeConditionState, error) {
	evidence := domain.ConditionEvidence{
		ConditionObservation: domain.ConditionObservation{
			Kind: row.Kind, State: domain.ConditionState(row.ObservedState),
			Used: row.UsedValue, Limit: row.LimitValue, Percent: row.PercentValue, Threshold: row.ThresholdValue,
			Unit: row.Unit, Resource: row.Resource, Scope: row.Scope, Source: row.Source, Message: row.Message,
			ObservedAt: time.UnixMicro(row.ObservedAt).UTC(), StaleAfter: time.UnixMicro(row.StaleAfter).UTC(),
		},
		ConsecutiveState: domain.ConditionState(row.ConsecutiveState),
		ConsecutiveCount: row.ConsecutiveCount,
		LastSuccessAt:    timeFromMicro(row.LastSuccessAt),
	}
	if row.EffectiveState != nil {
		state := domain.ConditionState(*row.EffectiveState)
		evidence.EffectiveState = &state
	}
	if !domain.ValidConditionEvidence(&evidence) {
		return domain.EdgeConditionState{}, fmt.Errorf("stored condition state is inconsistent: %w", ErrStorage)
	}
	return domain.EdgeConditionState{ConditionEvidence: evidence, Version: row.Version}, nil
}

func edgeConditionRowFromEvidence(monitorID, generation, configRevision int64, evidence domain.ConditionEvidence, version int64) edgeConditionRow {
	row := edgeConditionRow{
		MonitorID: monitorID, Generation: generation, Kind: evidence.Kind, ConfigRevision: configRevision,
		ObservedState: string(evidence.State), ConsecutiveState: string(evidence.ConsecutiveState),
		ConsecutiveCount: evidence.ConsecutiveCount,
		UsedValue:        evidence.Used, LimitValue: evidence.Limit, PercentValue: evidence.Percent, ThresholdValue: evidence.Threshold,
		Unit: evidence.Unit, Resource: evidence.Resource, Scope: evidence.Scope, Source: evidence.Source, Message: evidence.Message,
		ObservedAt: evidence.ObservedAt.UTC().UnixMicro(), StaleAfter: evidence.StaleAfter.UTC().UnixMicro(),
		LastSuccessAt: microFromTime(evidence.LastSuccessAt), Version: version,
	}
	if evidence.EffectiveState != nil {
		state := string(*evidence.EffectiveState)
		row.EffectiveState = &state
	}
	return row
}

// readEdgeConditionStates returns every evaluated condition for one assignment.
func readEdgeConditionStates(ctx context.Context, db bun.IDB, monitorID, generation int64) ([]domain.EdgeConditionState, error) {
	var rows []edgeConditionRow
	if err := db.NewSelect().Model(&rows).Where("monitor_id = ? AND generation = ?", monitorID, generation).Order("kind").Scan(ctx); err != nil {
		if errors.Is(storageError(ctx, err), ports.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]domain.EdgeConditionState, 0, len(rows))
	for _, row := range rows {
		state, err := row.state()
		if err != nil {
			return nil, err
		}
		out = append(out, state)
	}
	return out, nil
}

// applyEdgeConditionWork stores evaluated condition state changes, emits one
// ordered condition.transition event per promotion and returns the next free
// sequence number. Removals are fenced like updates and emit no event: the
// current-state projection's omission clears the hub mirror.
func (s *Store) applyEdgeConditionWork(ctx context.Context, tx bun.Tx, o domain.RegionalObservation, before []domain.EdgeConditionState, works []domain.EdgeConditionWork, seq int64) (int64, error) {
	if len(works) == 0 {
		return seq, nil
	}
	stored := make(map[string]domain.EdgeConditionState, len(before))
	for _, state := range before {
		stored[state.Kind] = state
	}
	seen := make(map[string]bool, len(works))
	for _, work := range works {
		kind := work.State.Kind
		if kind != domain.MonitorConditionSessionPool && kind != domain.MonitorConditionStorage || seen[kind] {
			return seq, domain.ErrValidation
		}
		seen[kind] = true
		prior, exists := stored[kind]
		var expected int64
		if exists {
			expected = prior.Version
		}
		if work.ExpectedVersion != expected {
			return seq, ports.ErrStaleLocalState
		}
		if work.Remove {
			if !exists {
				continue
			}
			if _, err := tx.NewDelete().Model((*edgeConditionRow)(nil)).Where("monitor_id = ? AND generation = ? AND kind = ?", o.MonitorID, o.AssignmentGeneration, kind).Exec(ctx); err != nil {
				return seq, err
			}
			continue
		}
		if !domain.ValidConditionEvidence(&work.State) || !sameRecordedRawCondition(o.Conditions, work.State.ConditionObservation) {
			return seq, domain.ErrValidation
		}
		if work.Transition != nil {
			transition := work.Transition
			if !domain.ValidConditionTransition(transition) || transition.MonitorID != o.MonitorID || transition.AssignmentGeneration != o.AssignmentGeneration ||
				transition.ConfigRevision != o.ConfigRevision || transition.Kind != kind || work.State.EffectiveState == nil || transition.State != *work.State.EffectiveState {
				return seq, domain.ErrValidation
			}
			if seq <= 0 {
				return seq, domain.ErrValidation
			}
			payload, err := s.telemetry.EncodeConditionTransition(seq, o.ObservedAt, *transition)
			if err != nil {
				return seq, err
			}
			if err := s.appendTelemetry(ctx, tx, seq, "condition.transition", o.ObservedAt, payload); err != nil {
				return seq, err
			}
			seq++
		}
		row := edgeConditionRowFromEvidence(o.MonitorID, o.AssignmentGeneration, o.ConfigRevision, work.State, expected+1)
		if !exists {
			if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
				return seq, err
			}
			continue
		}
		if _, err := tx.NewUpdate().Model(&row).WherePK().Exec(ctx); err != nil {
			return seq, fmt.Errorf("advance condition state: %w", err)
		}
	}
	return seq, nil
}
