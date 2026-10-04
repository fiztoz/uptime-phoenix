package services

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func capacitySample(kind string, state domain.ConditionState, percent float64, at time.Time) domain.ConditionObservation {
	threshold := 80.0
	return domain.ConditionObservation{
		Kind: kind, State: state, Used: &percent, Limit: floatPtr(100), Percent: &percent, Threshold: &threshold,
		Unit: "connections", Resource: "Session pool", Scope: "cluster", Source: "fixed engine query",
		Message: "sampled", ObservedAt: at,
	}
}

// evidencePrior adapts the last evaluation exactly like the durable edge rows.
func evidencePrior(t *testing.T, evaluations []ConditionEvaluation, kind string) *domain.MonitorCondition {
	t.Helper()
	states := make([]domain.EdgeConditionState, 0, len(evaluations))
	for i := range evaluations {
		states = append(states, domain.EdgeConditionState{ConditionEvidence: evaluations[i].State})
	}
	return ConditionStatePriors(states)[kind]
}

func evaluate(t *testing.T, evaluations []ConditionEvaluation, sample domain.ConditionObservation, at time.Time) ConditionEvaluation {
	t.Helper()
	evaluation := EvaluateCondition(evidencePrior(t, evaluations, sample.Kind), sample, 42, 60, at)
	if !domain.ValidConditionEvidence(&evaluation.State) {
		t.Fatalf("evaluator produced invalid evidence: %+v", evaluation.State)
	}
	return evaluation
}

func assertPromotion(t *testing.T, evaluation ConditionEvaluation, wantEffective *domain.ConditionState, wantCandidate domain.ConditionState, wantCount int, wantTransition bool) {
	t.Helper()
	got := evaluation.State
	if (got.EffectiveState == nil) != (wantEffective == nil) || got.EffectiveState != nil && *got.EffectiveState != *wantEffective {
		t.Fatalf("effective state: got %v want %v", got.EffectiveState, wantEffective)
	}
	if got.ConsecutiveState != wantCandidate || got.ConsecutiveCount != wantCount {
		t.Fatalf("candidate: got %s/%d want %s/%d", got.ConsecutiveState, got.ConsecutiveCount, wantCandidate, wantCount)
	}
	if (evaluation.Transition != nil) != wantTransition {
		t.Fatalf("transition presence: got %v want %v", evaluation.Transition, wantTransition)
	}
}

func TestEvaluateConditionTwoSamplePromotion(t *testing.T) {
	at := time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC)
	warning := domain.ConditionStateWarning
	ok := domain.ConditionStateOK
	errorState := domain.ConditionStateError

	t.Run("FirstWarningIsUnconfirmedUntilSecondSample", func(t *testing.T) {
		chain := make([]ConditionEvaluation, 0, 2)
		first := evaluate(t, chain, capacitySample(domain.MonitorConditionStorage, warning, 82, at), at)
		assertPromotion(t, first, nil, warning, 1, false)
		if first.State.Message == "" || first.State.LastSuccessAt == nil {
			t.Fatal("first sample lost measurement evidence")
		}
		chain = append(chain, first)
		second := evaluate(t, chain, capacitySample(domain.MonitorConditionStorage, warning, 82, at.Add(time.Minute)), at.Add(time.Minute))
		assertPromotion(t, second, &warning, warning, 2, true)
		if second.Transition == nil || second.Transition.PreviousState != nil || second.Transition.State != warning {
			t.Fatalf("first promotion must carry a null previous state: %+v", second.Transition)
		}
		chain = append(chain, second)
		third := evaluate(t, chain, capacitySample(domain.MonitorConditionStorage, warning, 84, at.Add(2*time.Minute)), at.Add(2*time.Minute))
		assertPromotion(t, third, &warning, warning, 3, false)
	})

	t.Run("RecoveryHysteresisCannotBeBypassed", func(t *testing.T) {
		var chain []ConditionEvaluation
		for i := 0; i < 2; i++ {
			next := evaluate(t, chain, capacitySample(domain.MonitorConditionStorage, warning, 82, at), at)
			chain = append(chain, next)
		}
		// 74 is below the 75 deadband: a recovery candidate starts.
		sample := evaluate(t, chain, capacitySample(domain.MonitorConditionStorage, ok, 74, at.Add(time.Minute)), at.Add(time.Minute))
		assertPromotion(t, sample, &warning, ok, 1, false)
		chain = append(chain, sample)
		// 77 is inside the deadband: hysteresis latches warning again and the
		// earlier recovery candidate is discarded, not counted.
		latch := evaluate(t, chain, capacitySample(domain.MonitorConditionStorage, ok, 77, at.Add(2*time.Minute)), at.Add(2*time.Minute))
		assertPromotion(t, latch, &warning, warning, 1, false)
		if latch.State.State != ok {
			t.Fatal("hysteresis must preserve the raw observed state")
		}
		if latch.State.Message == "sampled" {
			t.Fatal("hysteresis latch must explain why the condition stays warning")
		}
		chain = append(chain, latch)
		for i := 0; i < 2; i++ {
			recovery := evaluate(t, chain, capacitySample(domain.MonitorConditionStorage, ok, 74, at.Add(time.Duration(3+i)*time.Minute)), at.Add(time.Duration(3+i)*time.Minute))
			chain = append(chain, recovery)
			if i == 0 {
				assertPromotion(t, recovery, &warning, ok, 1, false)
			} else {
				assertPromotion(t, recovery, &ok, ok, 2, true)
				if recovery.Transition == nil || recovery.Transition.PreviousState == nil || *recovery.Transition.PreviousState != warning {
					t.Fatalf("recovery transition must name the confirmed previous state: %+v", recovery.Transition)
				}
			}
		}
	})

	t.Run("ErrorToWarningDoesNotBorrowCandidateCounts", func(t *testing.T) {
		var chain []ConditionEvaluation
		for i := 0; i < 2; i++ {
			next := evaluate(t, chain, capacitySample(domain.MonitorConditionSessionPool, errorState, 0, at), at)
			chain = append(chain, next)
			if i == 1 && (next.Transition == nil || next.Transition.State != errorState) {
				t.Fatal("second error sample must promote error")
			}
		}
		first := evaluate(t, chain, capacitySample(domain.MonitorConditionSessionPool, warning, 82, at), at)
		assertPromotion(t, first, &errorState, warning, 1, false)
		chain = append(chain, first)
		second := evaluate(t, chain, capacitySample(domain.MonitorConditionSessionPool, warning, 82, at), at)
		assertPromotion(t, second, &warning, warning, 2, true)
	})

	t.Run("BaselineAndEmptyStableStateConfirm", func(t *testing.T) {
		var chain []ConditionEvaluation
		first := evaluate(t, chain, capacitySample(domain.MonitorConditionStorage, ok, 10, at), at)
		assertPromotion(t, first, &ok, ok, 2, true)
		if first.Transition == nil || first.Transition.PreviousState != nil {
			t.Fatalf("baseline ok is the first promotion: %+v", first.Transition)
		}
		// A first warning followed by ok must not fabricate a stale unconfirmed
		// candidate: with no confirmed state, ok is the baseline again.
		unsettled := make([]ConditionEvaluation, 0, 1)
		warn := evaluate(t, unsettled, capacitySample(domain.MonitorConditionStorage, warning, 82, at), at)
		unsettled = append(unsettled, warn)
		recovered := evaluate(t, unsettled, capacitySample(domain.MonitorConditionStorage, ok, 10, at.Add(time.Minute)), at.Add(time.Minute))
		assertPromotion(t, recovered, &ok, ok, 2, true)
	})

	t.Run("RestartKeepsDebounceFromPersistedCandidate", func(t *testing.T) {
		var chain []ConditionEvaluation
		first := evaluate(t, chain, capacitySample(domain.MonitorConditionStorage, warning, 82, at), at)
		// The durable row (evidence + version) is what survives a restart.
		rows := []domain.EdgeConditionState{{ConditionEvidence: first.State, Version: 1}}
		prior := ConditionStatePriors(rows)[domain.MonitorConditionStorage]
		if prior == nil || prior.State != "" || prior.ConsecutiveState != warning || prior.ConsecutiveCount != 1 {
			t.Fatalf("persisted candidate lost: %+v", prior)
		}
		second := EvaluateCondition(prior, capacitySample(domain.MonitorConditionStorage, warning, 83, at.Add(time.Minute)), 42, 60, at.Add(time.Minute))
		assertPromotion(t, second, &warning, warning, 2, true)
	})
}
