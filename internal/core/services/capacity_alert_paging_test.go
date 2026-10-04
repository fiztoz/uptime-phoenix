package services

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func pagingFixture(t *testing.T) (domain.EdgeResolvedConfig, domain.EdgeResolvedAssignment) {
	t.Helper()
	monitor := &domain.Monitor{ID: 17, Name: "db", Type: "database", Active: true, Interval: 60, Config: map[string]any{"check_storage": true}}
	channel := &domain.Notification{ID: 10, Active: true, Type: "webhook"}
	assignment := domain.EdgeResolvedAssignment{
		Monitor: monitor, Generation: 1, EffectiveOwner: "ops",
		NotificationLinks: []domain.MonitorNotification{{MonitorID: 17, NotificationID: 10}},
	}
	config := domain.EdgeResolvedConfig{
		Metadata: domain.ProbeConfigMetadata{Revision: 1},
		Channels: map[int64]domain.EdgeResolvedChannel{10: {Notification: channel, Version: 1}},
	}
	return config, assignment
}

func confirmedEvidence(state domain.ConditionState, priorState domain.ConditionState, percent float64, at time.Time) (domain.ConditionEvidence, *domain.MonitorCondition) {
	threshold := 80.0
	evidence := domain.ConditionEvidence{
		ConditionObservation: domain.ConditionObservation{
			Kind: domain.MonitorConditionStorage, State: state, Percent: &percent, Threshold: &threshold,
			Unit: "bytes", Resource: "Database size", Scope: "database", Source: "fixed engine query",
			Message: "sampled", ObservedAt: at, StaleAfter: at.Add(3 * time.Minute),
		},
		ConsecutiveState: state, ConsecutiveCount: 2,
	}
	if state != domain.ConditionStateError {
		last := at
		evidence.LastSuccessAt = &last
	}
	confirmed := state
	evidence.EffectiveState = &confirmed
	previous := &domain.MonitorCondition{ConditionObservation: domain.ConditionObservation{Kind: domain.MonitorConditionStorage}}
	if priorState != "" {
		previous.State = priorState
	}
	return evidence, previous
}

func pagingInput(t *testing.T, state, priorState domain.ConditionState, prior *domain.EdgeConditionState, incident *domain.RegionalIncident, maintenance bool, at time.Time) CapacityPagingInput {
	t.Helper()
	config, assignment := pagingFixture(t)
	evidence, previous := confirmedEvidence(state, priorState, 84, at)
	return CapacityPagingInput{
		Config: &config, Monitor: assignment.Monitor, ProbeID: "probe", Assignment: assignment,
		Evidence: evidence, Previous: previous, Prior: prior, Incident: incident,
		Maintenance: maintenance, Now: at, NewID: testNewID,
	}
}

var idCounter int

func testNewID() (string, error) {
	idCounter++
	return "66666666-6666-4666-8666-" + string(rune('0'+idCounter%10)) + "0000000000", nil
}

func TestEvaluateCapacityPagingLifecycle(t *testing.T) {
	at := time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC)
	warning := domain.ConditionStateWarning
	errorState := domain.ConditionStateError
	ok := domain.ConditionStateOK

	t.Run("ConfirmedWarningOpensIncidentAndPages", func(t *testing.T) {
		work, err := EvaluateCapacityPaging(pagingInput(t, warning, ok, nil, nil, false, at))
		if err != nil || work == nil {
			t.Fatal(err, work)
		}
		if work.Incident == nil || work.Incident.Status != domain.AlertStatusFiring || work.Incident.TransitionVersion != 1 ||
			work.Incident.ConditionKind != domain.MonitorConditionStorage || !domain.ValidCapacityIncident(work.Incident) {
			t.Fatalf("incident identity: %+v", work.Incident)
		}
		if len(work.Intents) != 1 || work.Intents[0].EventKind != domain.DeliveryEventCapacityCondition ||
			work.Intents[0].SourceAlertID != work.Incident.SourceAlertID || work.Intents[0].SourceTransitionVersion != 1 {
			t.Fatalf("delivery work: %+v", work.Intents)
		}
		if work.DeliveredState != warning || work.OpenAlertID != work.Incident.SourceAlertID {
			t.Fatalf("cursor: %+v", work)
		}
		if !domain.ValidEdgeConditionAlertContent(work.Content) || work.Content.State != warning || work.Content.PreviousState != ok {
			t.Fatalf("rendered snapshot: %+v", work.Content)
		}
	})

	t.Run("UnconfirmedAndBaselineNeverPage", func(t *testing.T) {
		config, assignment := pagingFixture(t)
		evidence, previous := confirmedEvidence(warning, ok, 84, at)
		evidence.EffectiveState = nil
		work, err := EvaluateCapacityPaging(CapacityPagingInput{Config: &config, Monitor: assignment.Monitor, Assignment: assignment, Evidence: evidence, Previous: previous, Now: at, NewID: testNewID})
		if err != nil || work != nil {
			t.Fatal("unconfirmed candidate paged", work, err)
		}
		baseline, _ := confirmedEvidence(ok, "", 10, at)
		work, err = EvaluateCapacityPaging(CapacityPagingInput{Config: &config, Monitor: assignment.Monitor, Assignment: assignment, Evidence: baseline, Now: at, NewID: testNewID})
		if err != nil || work != nil {
			t.Fatal("baseline ok paged", work, err)
		}
	})

	t.Run("StateChangeRestatesIncidentAndRecoveryResolves", func(t *testing.T) {
		open, err := EvaluateCapacityPaging(pagingInput(t, warning, ok, nil, nil, false, at))
		if err != nil || open == nil {
			t.Fatal(err)
		}
		prior := &domain.EdgeConditionState{AlertSourceID: open.OpenAlertID, DeliveredState: warning}
		restated, err := EvaluateCapacityPaging(pagingInput(t, errorState, warning, prior, open.Incident, false, at.Add(time.Minute)))
		if err != nil || restated == nil {
			t.Fatal(err, restated)
		}
		if restated.Incident.SourceAlertID != open.Incident.SourceAlertID || restated.Incident.TransitionVersion != 2 ||
			restated.Incident.Status != domain.AlertStatusFiring || restated.Incident.StartedAt != open.Incident.StartedAt {
			t.Fatalf("restate broke identity: %+v", restated.Incident)
		}
		// Same state again: no new page.
		steady, err := EvaluateCapacityPaging(pagingInput(t, errorState, errorState, &domain.EdgeConditionState{AlertSourceID: open.OpenAlertID, DeliveredState: errorState}, restated.Incident, false, at.Add(2*time.Minute)))
		if err != nil || steady != nil {
			t.Fatal("steady state re-paged", steady, err)
		}
		recovered, err := EvaluateCapacityPaging(pagingInput(t, ok, errorState, &domain.EdgeConditionState{AlertSourceID: open.OpenAlertID, DeliveredState: errorState}, restated.Incident, false, at.Add(3*time.Minute)))
		if err != nil || recovered == nil {
			t.Fatal(err)
		}
		if recovered.Incident.Status != domain.AlertStatusResolved || recovered.Incident.ResolvedAt == nil ||
			recovered.Incident.TransitionVersion != 3 || recovered.OpenAlertID != "" || recovered.DeliveredState != ok {
			t.Fatalf("recovery lifecycle: %+v", recovered.Incident)
		}
		if len(recovered.Intents) != 1 || recovered.Content.State != ok {
			t.Fatalf("recovery page: %+v", recovered.Intents)
		}
		// Recovery without an open incident is inconsistent and fails loudly.
		if _, err := EvaluateCapacityPaging(pagingInput(t, ok, errorState, &domain.EdgeConditionState{DeliveredState: errorState}, nil, false, at.Add(4*time.Minute))); err == nil {
			t.Fatal("recovery without incident was accepted")
		}
	})

	t.Run("MaintenanceDefersThePageWithoutConsumingIt", func(t *testing.T) {
		work, err := EvaluateCapacityPaging(pagingInput(t, warning, ok, nil, nil, true, at))
		if err != nil || work != nil {
			t.Fatal("maintenance paged", work, err)
		}
		// The next check after the window still pages: suppression never advanced
		// the delivered cursor. The rendered snapshot repeats its state on both
		// sides exactly as the local service renders a delayed page.
		deferred, err := EvaluateCapacityPaging(pagingInput(t, warning, warning, nil, nil, false, at.Add(time.Hour)))
		if err != nil || deferred == nil {
			t.Fatal(err)
		}
		if deferred.Content.State != warning || deferred.Content.PreviousState != warning {
			t.Fatalf("delayed page snapshot: %+v", deferred.Content)
		}
	})

	t.Run("RemovalClosesOpenIncidentAdministratively", func(t *testing.T) {
		open, err := EvaluateCapacityPaging(pagingInput(t, warning, ok, nil, nil, false, at))
		if err != nil || open == nil {
			t.Fatal(err)
		}
		prior := &domain.EdgeConditionState{AlertSourceID: open.OpenAlertID, DeliveredState: warning, Alert: open.Incident}
		work := CloseCapacityAlertForRemoval(prior, "probe", 1, 1, at.Add(time.Minute))
		if work == nil || work.Incident == nil || work.Incident.Status != domain.AlertStatusResolved ||
			work.Incident.TransitionVersion != 2 || work.Incident.SourceAlertID != open.Incident.SourceAlertID ||
			len(work.Intents) != 0 || work.Content != nil || work.OpenAlertID != "" {
			t.Fatalf("administrative close: %+v", work)
		}
		if CloseCapacityAlertForRemoval(&domain.EdgeConditionState{}, "probe", 1, 1, at) != nil {
			t.Fatal("close invented work for a clean cursor")
		}
	})

	t.Run("DisabledChannelsStillUpdateTheCursor", func(t *testing.T) {
		input := pagingInput(t, warning, ok, nil, nil, false, at)
		input.Assignment.NotificationLinks = nil
		work, err := EvaluateCapacityPaging(input)
		if err != nil || work == nil {
			t.Fatal(err, work)
		}
		if len(work.Intents) != 0 || work.DeliveredState != warning || work.Content == nil {
			t.Fatalf("channel-less page: %+v", work)
		}
	})
}
