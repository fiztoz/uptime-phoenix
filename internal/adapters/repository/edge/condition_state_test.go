package edge

import (
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func conditionRecord(t *testing.T, at time.Time, expectedSeq, version int64, prior *domain.MonitorCondition, percent float64) domain.EdgeCheckRecord {
	t.Helper()
	i := testIdentity()
	threshold, limit := 80.0, 100.0
	used := percent
	sample := domain.ConditionObservation{
		Kind: domain.MonitorConditionStorage, State: domain.ConditionStateWarning, Used: &used, Limit: &limit, Percent: &percent, Threshold: &threshold,
		Unit: "bytes", Resource: "Database size", Scope: "database", Source: "fixed engine query",
		Message: "sampled", ObservedAt: at, StaleAfter: at.Add(3 * time.Minute),
	}
	evaluation := services.EvaluateCondition(prior, sample, 17, 60, at)
	if !domain.ValidConditionEvidence(&evaluation.State) {
		t.Fatalf("invalid evaluated evidence: %+v", evaluation.State)
	}
	work := domain.EdgeConditionWork{State: evaluation.State, ExpectedVersion: version}
	if evaluation.Transition != nil {
		transition := *evaluation.Transition
		transition.AssignmentGeneration = 1
		transition.ConfigRevision = 1
		work.Transition = &transition
	}
	return domain.EdgeCheckRecord{
		ExpectedStateSeq: expectedSeq,
		Observation: domain.RegionalObservation{
			MonitorID: 17, ProbeID: i.ProbeID, StreamID: i.StreamID, AssignmentGeneration: 1, ConfigRevision: 1,
			Status: domain.StatusUp, RawStatus: domain.StatusUp, Message: "HTTP 200",
			ObservedAt: at, ReceivedAt: at, Conditions: []domain.ConditionObservation{evaluation.State.ConditionObservation},
		},
		Conditions: []domain.EdgeConditionWork{work},
	}
}

func telemetryKinds(t *testing.T, s *Store) []string {
	t.Helper()
	var rows []struct {
		Kind string
		Seq  int64
	}
	if err := s.db.NewRaw("SELECT kind, seq FROM edge_telemetry_outbox ORDER BY seq").Scan(t.Context(), &rows); err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(rows))
	for _, row := range rows {
		kinds = append(kinds, row.Kind)
	}
	return kinds
}

func TestEdgeConditionLifecycleAndRestart(t *testing.T) {
	s, dir := testStore(t)
	s.telemetry = probe.EdgeTelemetryEncoder{}
	enroll(t, s)
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 1)); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	at := time.Now().UTC().Truncate(time.Microsecond)
	warning := domain.ConditionStateWarning

	// First sample: durable unconfirmed candidate, no transition event.
	first := conditionRecord(t, at, 0, 0, nil, 84)
	committed, err := s.CommitEdgeCheck(ctx, first)
	if err != nil || committed.Seq != 1 {
		t.Fatalf("first condition commit: %+v %v", committed, err)
	}
	evidence, err := s.ReadEdgeEvidence(ctx, 17, 1)
	if err != nil || len(evidence.Conditions) != 1 {
		t.Fatalf("condition state missing: %+v %v", evidence, err)
	}
	stored := evidence.Conditions[0]
	if stored.EffectiveState != nil || stored.ConsecutiveState != warning || stored.ConsecutiveCount != 1 || stored.Version != 1 {
		t.Fatalf("first sample must stay unconfirmed: %+v", stored)
	}
	if got := telemetryKinds(t, s); len(got) != 1 || got[0] != "observation" {
		t.Fatalf("unconfirmed sample emitted events: %v", got)
	}

	// Second sample promotes and emits exactly one ordered transition.
	prior := services.ConditionStatePriors(evidence.Conditions)[domain.MonitorConditionStorage]
	second := conditionRecord(t, at.Add(time.Minute), 1, 1, prior, 85)
	committed, err = s.CommitEdgeCheck(ctx, second)
	if err != nil || committed.Seq != 2 {
		t.Fatalf("second condition commit: %+v %v", committed, err)
	}
	if got := telemetryKinds(t, s); len(got) != 3 || got[1] != "observation" || got[2] != "condition.transition" {
		t.Fatalf("promotion events out of order: %v", got)
	}
	evidence, err = s.ReadEdgeEvidence(ctx, 17, 1)
	if err != nil || len(evidence.Conditions) != 1 {
		t.Fatalf("promoted state missing: %+v %v", evidence, err)
	}
	stored = evidence.Conditions[0]
	if stored.EffectiveState == nil || *stored.EffectiveState != warning || stored.Version != 2 {
		t.Fatalf("promotion not durable: %+v", stored)
	}

	// A superseded evaluation cannot commit.
	stale := conditionRecord(t, at.Add(2*time.Minute), 2, 1, prior, 86)
	if _, err := s.CommitEdgeCheck(ctx, stale); !errors.Is(err, ports.ErrStaleLocalState) {
		t.Fatalf("stale condition evaluation committed: %v", err)
	}

	// Restart retains the candidate so debounce survives a process cycle.
	reopened := certStoreReopen(t, dir)
	evidence, err = reopened.ReadEdgeEvidence(ctx, 17, 1)
	if err != nil || len(evidence.Conditions) != 1 || evidence.Conditions[0].Version != 2 {
		t.Fatalf("restart lost condition state: %+v %v", evidence, err)
	}

	// Disabling the check retires its row without inventing events.
	remove := domain.EdgeCheckRecord{
		ExpectedStateSeq: 2,
		Observation: domain.RegionalObservation{
			MonitorID: 17, ProbeID: testIdentity().ProbeID, StreamID: testIdentity().StreamID, AssignmentGeneration: 1, ConfigRevision: 1,
			Status: domain.StatusUp, RawStatus: domain.StatusUp, Message: "HTTP 200", ObservedAt: at.Add(3 * time.Minute), ReceivedAt: at.Add(3 * time.Minute),
		},
		Conditions: []domain.EdgeConditionWork{{State: domain.ConditionEvidence{ConditionObservation: domain.ConditionObservation{Kind: domain.MonitorConditionStorage}}, ExpectedVersion: 2, Remove: true}},
	}
	if _, err := reopened.CommitEdgeCheck(ctx, remove); err != nil {
		t.Fatal(err)
	}
	evidence, err = reopened.ReadEdgeEvidence(ctx, 17, 1)
	if err != nil || len(evidence.Conditions) != 0 {
		t.Fatalf("disabled check kept its row: %+v %v", evidence, err)
	}
	if got := telemetryKinds(t, s); len(got) != 4 || got[3] != "observation" {
		t.Fatalf("removal invented events: %v", got)
	}

	// Inconsistent transitions and duplicate kinds are rejected before storage.
	bad := conditionRecord(t, at.Add(4*time.Minute), 4, 0, nil, 84)
	bad.Conditions[0].Transition = &domain.ConditionTransition{MonitorID: 17, AssignmentGeneration: 1, ConfigRevision: 1, Kind: domain.MonitorConditionStorage, State: domain.ConditionStateError, Message: "mismatched"}
	if _, err := reopened.CommitEdgeCheck(ctx, bad); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("transition disagreeing with its evidence committed: %v", err)
	}
	bad.Conditions[0].Transition = nil
	bad.Conditions = append(bad.Conditions, bad.Conditions[0])
	if _, err := reopened.CommitEdgeCheck(ctx, bad); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("duplicate condition kind committed: %v", err)
	}
}
