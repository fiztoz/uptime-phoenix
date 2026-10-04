package repository

import (
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Storage widths of monitor_conditions text columns. The wire budget is larger,
// so mirrored values are truncated to what the schema can actually hold rather
// than failing an entire ingest batch on an oversize label.
const (
	conditionUnitWidth     = 24
	conditionResourceWidth = 32
	conditionScopeWidth    = 32
	conditionSourceWidth   = 160
)

// conditionSampleModel is the explicit storage DTO for one raw checker capacity
// measurement retained as history evidence. It carries no promotion or delivery
// state.
type conditionSampleModel struct {
	Kind       string    `json:"kind"`
	State      string    `json:"state"`
	Message    string    `json:"message"`
	Used       *float64  `json:"used"`
	Limit      *float64  `json:"limit"`
	Percent    *float64  `json:"percent"`
	Threshold  *float64  `json:"threshold"`
	Unit       string    `json:"unit"`
	Resource   string    `json:"resource"`
	Scope      string    `json:"scope"`
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
	StaleAfter time.Time `json:"stale_after"`
}

// conditionEvidenceModel is the explicit storage DTO for one source-evaluated
// condition: raw measurement, promoted state and the pending transition
// candidate. Domain structs are never serialized.
type conditionEvidenceModel struct {
	Kind             string     `json:"kind"`
	ObservedState    string     `json:"observed_state"`
	EffectiveState   *string    `json:"effective_state"`
	ConsecutiveState string     `json:"consecutive_state"`
	ConsecutiveCount int        `json:"consecutive_count"`
	Message          string     `json:"message"`
	Used             *float64   `json:"used"`
	Limit            *float64   `json:"limit"`
	Percent          *float64   `json:"percent"`
	Threshold        *float64   `json:"threshold"`
	Unit             string     `json:"unit"`
	Resource         string     `json:"resource"`
	Scope            string     `json:"scope"`
	Source           string     `json:"source"`
	ObservedAt       time.Time  `json:"observed_at"`
	StaleAfter       time.Time  `json:"stale_after"`
	LastSuccessAt    *time.Time `json:"last_success_at"`
}

func truncateWidth(value string, width int) string {
	if len(value) <= width {
		return value
	}
	// Cut on the widest rune prefix that fits the byte budget.
	cut := width
	for cut > 0 && !isUTF8Start(value[cut]) {
		cut--
	}
	return value[:cut]
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

func conditionSampleModelFromDomain(sample domain.ConditionObservation) conditionSampleModel {
	return conditionSampleModel{
		Kind: sample.Kind, State: string(sample.State), Message: sample.Message,
		Used: sample.Used, Limit: sample.Limit, Percent: sample.Percent, Threshold: sample.Threshold,
		Unit: truncateWidth(sample.Unit, conditionUnitWidth), Resource: truncateWidth(sample.Resource, conditionResourceWidth),
		Scope: truncateWidth(sample.Scope, conditionScopeWidth), Source: truncateWidth(sample.Source, conditionSourceWidth),
		ObservedAt: sample.ObservedAt.UTC(), StaleAfter: sample.StaleAfter.UTC(),
	}
}

func conditionSamplesFromDomain(samples []domain.ConditionObservation) []conditionSampleModel {
	if len(samples) == 0 {
		return nil
	}
	out := make([]conditionSampleModel, 0, len(samples))
	for _, sample := range samples {
		out = append(out, conditionSampleModelFromDomain(sample))
	}
	return out
}

func (m conditionSampleModel) sample() domain.ConditionObservation {
	return domain.ConditionObservation{
		Kind: m.Kind, State: domain.ConditionState(m.State), Message: m.Message,
		Used: m.Used, Limit: m.Limit, Percent: m.Percent, Threshold: m.Threshold,
		Unit: m.Unit, Resource: m.Resource, Scope: m.Scope, Source: m.Source,
		ObservedAt: m.ObservedAt.UTC(), StaleAfter: m.StaleAfter.UTC(),
	}
}

func conditionSamplesDomain(models []conditionSampleModel) []domain.ConditionObservation {
	out := make([]domain.ConditionObservation, 0, len(models))
	for _, model := range models {
		out = append(out, model.sample())
	}
	return out
}

func conditionEvidenceModelFromDomain(evidence domain.ConditionEvidence) conditionEvidenceModel {
	model := conditionEvidenceModel{
		Kind: evidence.Kind, ObservedState: string(evidence.State),
		ConsecutiveState: string(evidence.ConsecutiveState), ConsecutiveCount: evidence.ConsecutiveCount,
		Message: evidence.Message,
		Used:    evidence.Used, Limit: evidence.Limit, Percent: evidence.Percent, Threshold: evidence.Threshold,
		Unit: truncateWidth(evidence.Unit, conditionUnitWidth), Resource: truncateWidth(evidence.Resource, conditionResourceWidth),
		Scope: truncateWidth(evidence.Scope, conditionScopeWidth), Source: truncateWidth(evidence.Source, conditionSourceWidth),
		ObservedAt: evidence.ObservedAt.UTC(), StaleAfter: evidence.StaleAfter.UTC(),
	}
	if evidence.EffectiveState != nil {
		state := string(*evidence.EffectiveState)
		model.EffectiveState = &state
	}
	if evidence.LastSuccessAt != nil {
		at := evidence.LastSuccessAt.UTC()
		model.LastSuccessAt = &at
	}
	return model
}

func conditionEvidenceFromModel(model conditionEvidenceModel) domain.ConditionEvidence {
	evidence := domain.ConditionEvidence{
		ConditionObservation: domain.ConditionObservation{
			Kind: model.Kind, State: domain.ConditionState(model.ObservedState), Message: model.Message,
			Used: model.Used, Limit: model.Limit, Percent: model.Percent, Threshold: model.Threshold,
			Unit: model.Unit, Resource: model.Resource, Scope: model.Scope, Source: model.Source,
			ObservedAt: model.ObservedAt.UTC(), StaleAfter: model.StaleAfter.UTC(),
		},
		ConsecutiveState: domain.ConditionState(model.ConsecutiveState),
		ConsecutiveCount: model.ConsecutiveCount,
	}
	if model.EffectiveState != nil {
		state := domain.ConditionState(*model.EffectiveState)
		evidence.EffectiveState = &state
	}
	if model.LastSuccessAt != nil {
		at := model.LastSuccessAt.UTC()
		evidence.LastSuccessAt = &at
	}
	return evidence
}

func conditionEvidenceListFromDomain(list []domain.ConditionEvidence) []conditionEvidenceModel {
	if len(list) == 0 {
		return nil
	}
	out := make([]conditionEvidenceModel, 0, len(list))
	for i := range list {
		out = append(out, conditionEvidenceModelFromDomain(list[i]))
	}
	return out
}

func conditionEvidenceListDomain(models []conditionEvidenceModel) []domain.ConditionEvidence {
	out := make([]domain.ConditionEvidence, 0, len(models))
	for _, model := range models {
		out = append(out, conditionEvidenceFromModel(model))
	}
	return out
}

// sameConditionEvidence compares stored evaluated rows with an incoming
// same-sequence projection. Immutable identity must not change at equal source
// sequence, so any difference is a conflict rather than a silent rewrite.
func sameConditionEvidence(stored []conditionEvidenceModel, incoming []domain.ConditionEvidence) bool {
	if len(stored) != len(incoming) {
		return false
	}
	matched := make([]bool, len(stored))
	for i := range incoming {
		candidate := conditionEvidenceModelFromDomain(incoming[i])
		found := false
		for j := range stored {
			if matched[j] || !conditionEvidenceModelEqual(stored[j], candidate) {
				continue
			}
			matched[j], found = true, true
			break
		}
		if !found {
			return false
		}
	}
	return true
}

func conditionEvidenceModelEqual(a, b conditionEvidenceModel) bool {
	if a.Kind != b.Kind || a.ObservedState != b.ObservedState || a.ConsecutiveState != b.ConsecutiveState ||
		a.ConsecutiveCount != b.ConsecutiveCount || a.Message != b.Message || a.Unit != b.Unit ||
		a.Resource != b.Resource || a.Scope != b.Scope || a.Source != b.Source ||
		!a.ObservedAt.Equal(b.ObservedAt) || !a.StaleAfter.Equal(b.StaleAfter) ||
		(a.EffectiveState == nil) != (b.EffectiveState == nil) || (a.LastSuccessAt == nil) != (b.LastSuccessAt == nil) {
		return false
	}
	if a.EffectiveState != nil && *a.EffectiveState != *b.EffectiveState {
		return false
	}
	if a.LastSuccessAt != nil && !a.LastSuccessAt.Equal(*b.LastSuccessAt) {
		return false
	}
	for _, pair := range [][2]*float64{{a.Used, b.Used}, {a.Limit, b.Limit}, {a.Percent, b.Percent}, {a.Threshold, b.Threshold}} {
		left, right := pair[0], pair[1]
		if (left == nil) != (right == nil) {
			return false
		}
		if left != nil && *left != *right {
			return false
		}
	}
	return true
}
