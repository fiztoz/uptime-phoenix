package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// edgeConditionRow is the durable, source-owned evaluated auxiliary condition
// for one immutable assignment generation and condition kind. Times follow the
// edge microsecond storage contract. DeliveredState is the coarse paging cursor
// and AlertSourceID points at the single open capacity incident.
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
	AlertSourceID    string
	DeliveredState   string
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
	return domain.EdgeConditionState{
		ConditionEvidence: evidence,
		AlertSourceID:     row.AlertSourceID,
		DeliveredState:    domain.ConditionState(row.DeliveredState),
		Version:           row.Version,
	}, nil
}

func edgeConditionRowFromEvidence(monitorID, generation, configRevision int64, evidence domain.ConditionEvidence, alert *domain.EdgeConditionAlertWork, version int64) edgeConditionRow {
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
	if alert != nil {
		row.AlertSourceID = alert.OpenAlertID
		row.DeliveredState = string(alert.DeliveredState)
	}
	return row
}

// readEdgeConditionStates returns every evaluated condition for one assignment
// with its open capacity incident resolved from the same read snapshot.
func readEdgeConditionStates(ctx context.Context, db bun.IDB, probeID string, monitorID, generation int64) ([]domain.EdgeConditionState, error) {
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
		if row.AlertSourceID != "" {
			var incident edgeIncidentRow
			if err := db.NewSelect().Model(&incident).Where("source_alert_id = ? AND monitor_id = ? AND generation = ?", row.AlertSourceID, monitorID, generation).Scan(ctx); err != nil {
				return nil, err
			}
			open := incident.incident(probeID)
			if open.SubjectKind != domain.IncidentSubjectCapacity || open.ConditionKind != row.Kind || open.Status != domain.AlertStatusFiring {
				return nil, fmt.Errorf("condition cursor references a non-capacity incident: %w", ErrStorage)
			}
			state.Alert = open
		}
		out = append(out, state)
	}
	return out, nil
}

// applyEdgeConditionWork stores evaluated condition state changes and applies
// the capacity paging lifecycle: one ordered alert.transition per incident
// transition, its delivery work with an immutable rendered snapshot, then one
// ordered condition.transition per promotion. The paging cursor advances with
// the same fenced row write, so a concurrent writer forces re-evaluation.
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
			seq, err := s.applyEdgeConditionAlertWork(ctx, tx, o, kind, prior, work.Alert, seq)
			if err != nil {
				return seq, err
			}
			if _, err := tx.NewDelete().Model((*edgeConditionRow)(nil)).Where("monitor_id = ? AND generation = ? AND kind = ?", o.MonitorID, o.AssignmentGeneration, kind).Exec(ctx); err != nil {
				return seq, err
			}
			continue
		}
		if !domain.ValidConditionEvidence(&work.State) || !sameRecordedRawCondition(o.Conditions, work.State.ConditionObservation) {
			return seq, domain.ErrValidation
		}
		seq, err := s.applyEdgeConditionAlertWork(ctx, tx, o, kind, prior, work.Alert, seq)
		if err != nil {
			return seq, err
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
		row := edgeConditionRowFromEvidence(o.MonitorID, o.AssignmentGeneration, o.ConfigRevision, work.State, work.Alert, expected+1)
		if work.Alert == nil && exists {
			row.AlertSourceID, row.DeliveredState = prior.AlertSourceID, string(prior.DeliveredState)
		}
		if work.Alert == nil && !exists {
			row.AlertSourceID, row.DeliveredState = "", ""
		}
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

// applyEdgeConditionAlertWork applies one capacity paging lifecycle: the
// ordered incident transition, its durable delivery intents with an immutable
// rendered snapshot, and the next free sequence number. Removal may carry an
// administrative resolution without any provider work.
func (s *Store) applyEdgeConditionAlertWork(ctx context.Context, tx bun.Tx, o domain.RegionalObservation, kind string, prior domain.EdgeConditionState, work *domain.EdgeConditionAlertWork, seq int64) (int64, error) {
	if work == nil {
		return seq, nil
	}
	incident := work.Incident
	if incident == nil || !domain.ValidCapacityIncident(incident) || incident.MonitorID != o.MonitorID ||
		incident.AssignmentGeneration != o.AssignmentGeneration || incident.ConfigRevision != o.ConfigRevision ||
		incident.ProbeID != o.ProbeID || incident.ConditionKind != kind || incident.StartedAt.IsZero() ||
		(work.Content != nil && work.Content.Kind != kind) {
		return seq, domain.ErrValidation
	}
	if work.DeliveredState != "" && !work.DeliveredState.IsValid() || work.DeliveredState == "" && incident.Status == domain.AlertStatusResolved && work.OpenAlertID != "" {
		return seq, domain.ErrValidation
	}
	if incident.Status == domain.AlertStatusFiring && work.OpenAlertID != incident.SourceAlertID || incident.Status == domain.AlertStatusResolved && work.OpenAlertID != "" {
		return seq, domain.ErrValidation
	}
	if err := saveEdgeCapacityIncident(ctx, tx, prior.Alert, *incident); err != nil {
		return seq, err
	}
	payload, err := s.telemetry.EncodeIncident(seq, o.ObservedAt, *incident)
	if err != nil {
		return seq, err
	}
	if err := s.appendTelemetry(ctx, tx, seq, "alert.transition", o.ObservedAt, payload); err != nil {
		return seq, err
	}
	seq++
	if len(work.Intents) > 0 {
		if work.Content == nil {
			return seq, domain.ErrValidation
		}
		for _, intent := range work.Intents {
			if err := insertEdgeCapacityIntent(ctx, tx, o, *incident, intent, work.Content); err != nil {
				return seq, err
			}
		}
	}
	return seq, nil
}

// saveEdgeCapacityIncident writes one capacity incident version. A firing row is
// always a new identity at version one; a restate or resolution may only advance
// the stored incident this cursor points at and keeps its immutable condition
// kind and start time.
func saveEdgeCapacityIncident(ctx context.Context, tx bun.Tx, prior *domain.RegionalIncident, inc domain.RegionalIncident) error {
	if !domain.ValidCapacityIncident(&inc) || inc.StartedAt.IsZero() {
		return domain.ErrValidation
	}
	row := newEdgeIncidentRow(inc)
	switch {
	case inc.Status == domain.AlertStatusFiring && inc.TransitionVersion == 1:
		// A firing row is always a new identity at version one.
		if inc.ResolvedAt != nil || inc.AckedAt != nil || inc.AckCommandID != "" || inc.AckActorDisplayName != "" || inc.AckNote != nil {
			return domain.ErrValidation
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return fmt.Errorf("insert capacity incident: %w", err)
		}
		return nil
	case inc.Status == domain.AlertStatusFiring || inc.Status == domain.AlertStatusResolved:
		// A restate or resolution may only advance the stored incident this cursor
		// points at and keeps its immutable condition kind and start time.
		if prior == nil || prior.SourceAlertID != inc.SourceAlertID || prior.ConditionKind != inc.ConditionKind ||
			prior.StartedAt.UTC().UnixMicro() != inc.StartedAt.UTC().UnixMicro() || prior.Status != domain.AlertStatusFiring ||
			inc.TransitionVersion != prior.TransitionVersion+1 || !sameIncidentAcknowledgement(inc, *prior) ||
			inc.Status == domain.AlertStatusResolved && inc.ResolvedAt == nil || inc.Status == domain.AlertStatusFiring && inc.ResolvedAt != nil {
			return domain.ErrValidation
		}
		if _, err := tx.NewUpdate().Model(&row).WherePK().Exec(ctx); err != nil {
			return fmt.Errorf("advance capacity incident: %w", err)
		}
		return nil
	default:
		return domain.ErrValidation
	}
}

// insertEdgeCapacityIntent records durable source work for one capacity page.
// The rendered snapshot is stored with the row so a retry after a restart
// delivers the state it was committed for.
func insertEdgeCapacityIntent(ctx context.Context, tx bun.Tx, o domain.RegionalObservation, inc domain.RegionalIncident, intent domain.DeliveryIntent, content *domain.EdgeConditionAlertContent) error {
	if !domain.ValidEdgeConditionAlertContent(content) || content.Kind != inc.ConditionKind ||
		inc.Status != domain.AlertStatusFiring && inc.Status != domain.AlertStatusResolved ||
		inc.Status == domain.AlertStatusResolved && inc.TransitionVersion == 1 {
		return domain.ErrValidation
	}
	if err := validateEdgeDeliveryIdentity(o, &inc, intent); err != nil {
		return err
	}
	if intent.EventKind != domain.DeliveryEventCapacityCondition {
		return domain.ErrValidation
	}
	return insertEdgeQueuedDelivery(ctx, tx, domain.QueuedDelivery{
		DeliveryIntent: intent, MonitorID: o.MonitorID, AssignmentGeneration: o.AssignmentGeneration, StreamID: o.StreamID,
		SourceSeq: o.Seq, ConfigRevision: o.ConfigRevision, CheckStatus: o.Status, CheckOutput: content.Message,
		ObservedAt: o.ObservedAt, IncidentStatus: inc.Status, StartedAt: inc.StartedAt, ResolvedAt: inc.ResolvedAt,
		CreatedAt: o.ReceivedAt, Condition: content,
	})
}

// conditionContentModel is the explicit storage DTO for the immutable rendered
// capacity snapshot retained with a delivery intent.
type conditionContentModel struct {
	Kind          string    `json:"kind"`
	State         string    `json:"state"`
	PreviousState string    `json:"previous_state"`
	Used          *float64  `json:"used"`
	Limit         *float64  `json:"limit"`
	Percent       *float64  `json:"percent"`
	Threshold     *float64  `json:"threshold"`
	Unit          string    `json:"unit"`
	Resource      string    `json:"resource"`
	Scope         string    `json:"scope"`
	Source        string    `json:"source"`
	Message       string    `json:"message"`
	ObservedAt    time.Time `json:"observed_at"`
}

func marshalConditionContent(content *domain.EdgeConditionAlertContent) (string, error) {
	if !domain.ValidEdgeConditionAlertContent(content) {
		return "", domain.ErrValidation
	}
	data, err := json.Marshal(conditionContentModel{
		Kind: content.Kind, State: string(content.State), PreviousState: string(content.PreviousState),
		Used: content.Used, Limit: content.Limit, Percent: content.Percent, Threshold: content.Threshold,
		Unit: content.Unit, Resource: content.Resource, Scope: content.Scope, Source: content.Source,
		Message: content.Message, ObservedAt: content.ObservedAt.UTC(),
	})
	if err != nil {
		return "", domain.ErrValidation
	}
	return string(data), nil
}

func unmarshalConditionContent(raw string) *domain.EdgeConditionAlertContent {
	if raw == "" {
		return nil
	}
	var model conditionContentModel
	if err := json.Unmarshal([]byte(raw), &model); err != nil {
		return nil
	}
	content := &domain.EdgeConditionAlertContent{
		Kind: model.Kind, State: domain.ConditionState(model.State), PreviousState: domain.ConditionState(model.PreviousState),
		Used: model.Used, Limit: model.Limit, Percent: model.Percent, Threshold: model.Threshold,
		Unit: model.Unit, Resource: model.Resource, Scope: model.Scope, Source: model.Source,
		Message: model.Message, ObservedAt: model.ObservedAt.UTC(),
	}
	if !domain.ValidEdgeConditionAlertContent(content) {
		return nil
	}
	return content
}
