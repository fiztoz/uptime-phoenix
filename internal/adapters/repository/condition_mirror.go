package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// The remote monitor_conditions mirror is written by three source-evidenced
// paths and never re-evaluates promotion: raw samples refresh measurements and
// freshness, condition.transition events carry promoted state, and the
// current-state snapshot replaces the complete evaluated row set. The source_seq
// fence keeps the highest source sequence authoritative, so older replay,
// retired assignment history and stale sessions cannot replace newer state.

func loadRemoteCondition(ctx context.Context, tx bun.Tx, monitorID int64, probeID string, generation int64, kind string) (*MonitorConditionModel, bool, error) {
	row := new(MonitorConditionModel)
	err := tx.NewSelect().Model(row).
		Where("monitor_id = ? AND probe_id = ? AND assignment_generation = ? AND kind = ?", monitorID, probeID, generation, kind).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return row, true, nil
}

func saveRemoteCondition(ctx context.Context, tx bun.Tx, row *MonitorConditionModel, exists bool) error {
	if exists {
		_, err := tx.NewUpdate().Model(row).WherePK().Exec(ctx)
		return err
	}
	_, err := tx.NewInsert().Model(row).Exec(ctx)
	return err
}

func remoteConditionEvidenceFound(ctx context.Context, tx bun.Tx, monitorID int64, probeID string, generation int64, kind string) (bool, error) {
	_, exists, err := loadRemoteCondition(ctx, tx, monitorID, probeID, generation, kind)
	return exists, err
}

// syncRemoteConditionMeasurements refreshes measurement, message and freshness
// columns from one accepted raw sample. It never touches promoted state: a new
// row starts unconfirmed with the observed candidate, exactly like the source's
// first sample, and promotion arrives later as a transition or snapshot.
func syncRemoteConditionMeasurements(ctx context.Context, tx bun.Tx, obs domain.RegionalObservation) error {
	if obs.ProbeID == domain.LocalProbeID || obs.MonitorID <= 0 || obs.AssignmentGeneration <= 0 {
		return nil
	}
	for _, sample := range obs.Conditions {
		row, exists, err := loadRemoteCondition(ctx, tx, obs.MonitorID, obs.ProbeID, obs.AssignmentGeneration, sample.Kind)
		if err != nil {
			return err
		}
		if exists && row.SourceSeq >= obs.Seq {
			continue
		}
		model := conditionSampleModelFromDomain(sample)
		if !exists {
			row = &MonitorConditionModel{
				MonitorID: obs.MonitorID, ProbeID: obs.ProbeID, AssignmentGeneration: obs.AssignmentGeneration,
				Kind: sample.Kind, State: "", ConsecutiveState: domain.ConditionState(model.State), ConsecutiveCount: 1,
			}
		}
		row.UsedValue, row.LimitValue, row.PercentValue, row.ThresholdValue = model.Used, model.Limit, model.Percent, model.Threshold
		row.Unit, row.Resource, row.Scope, row.Source = model.Unit, model.Resource, model.Scope, model.Source
		row.Message, row.ObservedAt, row.StaleAfter = model.Message, sample.ObservedAt.UTC(), sample.StaleAfter.UTC()
		if sample.State != domain.ConditionStateError {
			at := sample.ObservedAt.UTC()
			row.LastSuccessAt = &at
		}
		row.SourceSeq = obs.Seq
		if err := saveRemoteCondition(ctx, tx, row, exists); err != nil {
			return err
		}
	}
	return nil
}

// mirrorRemoteConditionTransition applies one promoted source transition. The
// promoted state is copied, never derived: no candidate recount, no hysteresis,
// no notification cursor. Measurement freshness stays owned by raw samples.
func mirrorRemoteConditionTransition(ctx context.Context, tx bun.Tx, probeID string, generation, monitorID, seq int64, transition domain.ConditionTransition) error {
	if probeID == domain.LocalProbeID {
		return nil
	}
	row, exists, err := loadRemoteCondition(ctx, tx, monitorID, probeID, generation, transition.Kind)
	if err != nil {
		return err
	}
	if !exists {
		// Raw evidence always precedes its promotion in the stream. A transition
		// without mirrored evidence is rejected before persistence.
		return ports.ErrNotFound
	}
	if row.SourceSeq >= seq {
		return nil
	}
	row.State = transition.State
	row.ConsecutiveState = transition.State
	if row.ConsecutiveCount < 2 {
		row.ConsecutiveCount = 2
	}
	row.Message = transition.Message
	row.SourceSeq = seq
	return saveRemoteCondition(ctx, tx, row, true)
}

// replaceRemoteConditions mirrors one accepted current-state projection: every
// listed kind is replaced wholesale and any omitted kind is cleared, so a
// complete snapshot omission retires the assignment's capacity projection while
// retaining historical samples.
func replaceRemoteConditions(ctx context.Context, tx bun.Tx, state domain.RegionalState) error {
	if state.ProbeID == domain.LocalProbeID || state.MonitorID <= 0 || state.AssignmentGeneration <= 0 {
		return nil
	}
	listed := make(map[string]domain.ConditionEvidence, len(state.Conditions))
	for i := range state.Conditions {
		listed[state.Conditions[i].Kind] = state.Conditions[i]
	}
	var rows []*MonitorConditionModel
	if err := tx.NewSelect().Model(&rows).
		Where("monitor_id = ? AND probe_id = ? AND assignment_generation = ?", state.MonitorID, state.ProbeID, state.AssignmentGeneration).
		Scan(ctx); err != nil {
		return err
	}
	for _, row := range rows {
		if _, ok := listed[row.Kind]; !ok && row.SourceSeq <= state.Seq {
			if _, err := tx.NewDelete().Model(row).WherePK().Exec(ctx); err != nil {
				return err
			}
		}
	}
	for _, evidence := range listed {
		row, exists, err := loadRemoteCondition(ctx, tx, state.MonitorID, state.ProbeID, state.AssignmentGeneration, evidence.Kind)
		if err != nil {
			return err
		}
		if exists && row.SourceSeq > state.Seq {
			continue
		}
		model := conditionEvidenceModelFromDomain(evidence)
		if !exists {
			row = &MonitorConditionModel{MonitorID: state.MonitorID, ProbeID: state.ProbeID, AssignmentGeneration: state.AssignmentGeneration, Kind: evidence.Kind}
		}
		row.State = domain.ConditionState("")
		if model.EffectiveState != nil {
			row.State = domain.ConditionState(*model.EffectiveState)
		}
		row.ConsecutiveState, row.ConsecutiveCount = domain.ConditionState(model.ConsecutiveState), model.ConsecutiveCount
		row.UsedValue, row.LimitValue, row.PercentValue, row.ThresholdValue = model.Used, model.Limit, model.Percent, model.Threshold
		row.Unit, row.Resource, row.Scope, row.Source = model.Unit, model.Resource, model.Scope, model.Source
		row.Message, row.ObservedAt, row.StaleAfter, row.LastSuccessAt = model.Message, model.ObservedAt, model.StaleAfter, model.LastSuccessAt
		row.SourceSeq = state.Seq
		if err := saveRemoteCondition(ctx, tx, row, exists); err != nil {
			return err
		}
	}
	return nil
}

// clearRemoteConditions retires the mirrored capacity projection of one
// assignment when its current state is removed.
func clearRemoteConditions(ctx context.Context, tx bun.Tx, monitorID int64, probeID string, generation int64) error {
	_, err := tx.NewDelete().Model((*MonitorConditionModel)(nil)).
		Where("monitor_id = ? AND probe_id = ? AND assignment_generation = ?", monitorID, probeID, generation).Exec(ctx)
	return err
}
