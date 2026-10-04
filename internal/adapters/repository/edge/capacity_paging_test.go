package edge

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func capacityAssignment(enabled bool) (*domain.EdgeResolvedConfig, domain.EdgeResolvedAssignment) {
	monitor := &domain.Monitor{ID: 17, Name: "db", Type: "database", Active: true, Interval: 60, Config: map[string]any{"check_storage": enabled}}
	links := []domain.MonitorNotification{{MonitorID: 17, NotificationID: 10}}
	a := domain.EdgeResolvedAssignment{Monitor: monitor, Generation: 1, EffectiveOwner: "ops", NotificationLinks: links}
	c := &domain.EdgeResolvedConfig{
		Metadata:    domain.ProbeConfigMetadata{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: testHubID, ProbeID: testIdentity().ProbeID}, Revision: 1},
		Assignments: []domain.EdgeResolvedAssignment{a},
		Maintenance: map[int64]*domain.MaintenanceWindow{},
		Channels: map[int64]domain.EdgeResolvedChannel{
			10: {Notification: &domain.Notification{ID: 10, Type: "webhook", Active: true}, Version: 1},
		},
	}
	return c, a
}

func capacityResult(state domain.ConditionState, percent float64) ports.CheckResult {
	threshold, limit := 80.0, 100.0
	used := percent
	return ports.CheckResult{
		Status: domain.StatusUp, LatencyMs: 5, DurationMs: 7, Message: "select 1",
		Conditions: []domain.ConditionObservation{{
			Kind: domain.MonitorConditionStorage, State: state, Used: &used, Limit: &limit, Percent: &percent, Threshold: &threshold,
			Unit: "bytes", Resource: "Database size", Scope: "database", Source: "fixed engine query", Message: "sampled",
		}},
	}
}

func TestEdgeCapacityPagingLifecycle(t *testing.T) {
	s, dir := testStore(t)
	s.telemetry = probe.EdgeTelemetryEncoder{}
	enroll(t, s)
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 1)); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	at := time.Now().UTC().Truncate(time.Microsecond)
	stage := "setup"
	record := func(svcStore *Store, config *domain.EdgeResolvedConfig, assignment domain.EdgeResolvedAssignment, result ports.CheckResult, when time.Time) {
		t.Helper()
		if _, err := services.NewEdgeRecordingService(svcStore, svcStore, nil).Record(ctx, config, assignment, result, when); err != nil {
			t.Fatal(stage, err)
		}
	}
	warning := domain.ConditionStateWarning
	errorState := domain.ConditionStateError
	ok := domain.ConditionStateOK

	c, a := capacityAssignment(true)

	// First sample: unconfirmed, no page.
	stage = "first"
	record(s, c, a, capacityResult(warning, 84), at)
	evidence, err := s.ReadEdgeEvidence(ctx, 17, 1)
	if err != nil || len(evidence.Conditions) != 1 || evidence.Conditions[0].EffectiveState != nil || evidence.Conditions[0].Alert != nil {
		t.Fatalf("first sample paged or lost state: %+v %v", evidence.Conditions, err)
	}
	if got := telemetryKinds(t, s); len(got) != 1 || got[0] != "observation" {
		t.Fatalf("unconfirmed sample emitted events: %v", got)
	}

	// Second sample confirms and pages: observation, then the capacity incident,
	// then the promotion transition referencing it.
	stage = "confirm"
	record(s, c, a, capacityResult(warning, 85), at.Add(time.Minute))
	if got := telemetryKinds(t, s); len(got) != 4 || got[1] != "observation" || got[2] != "alert.transition" || got[3] != "condition.transition" {
		t.Fatalf("paging events out of order: %v", got)
	}
	evidence, err = s.ReadEdgeEvidence(ctx, 17, 1)
	if err != nil || evidence.Conditions[0].Alert == nil || evidence.Conditions[0].Alert.Status != domain.AlertStatusFiring ||
		evidence.Conditions[0].Alert.ConditionKind != domain.MonitorConditionStorage || evidence.Conditions[0].DeliveredState != warning {
		t.Fatalf("open incident or cursor missing: %+v %v", evidence.Conditions[0], err)
	}
	if evidence.Conditions[0].AlertSourceID != evidence.Conditions[0].Alert.SourceAlertID {
		t.Fatal("cursor lost its open incident", evidence.Conditions[0])
	}
	var intents int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_delivery_outbox WHERE event_kind = 'capacity_condition'").Scan(ctx, &intents); err != nil || intents != 1 {
		t.Fatal("capacity page not queued", intents, err)
	}
	var contentJSON string
	if err := s.db.NewRaw("SELECT condition_json FROM edge_delivery_outbox WHERE event_kind = 'capacity_condition'").Scan(ctx, &contentJSON); err != nil {
		t.Fatal(err)
	}
	if content := unmarshalConditionContent(contentJSON); content == nil || content.State != warning || content.PreviousState != ok {
		t.Fatalf("immutable snapshot lost: %+v", content)
	}

	// A state change re-pages only after its own two-sample confirmation and
	// restates the same incident identity.
	firstID := evidence.Conditions[0].Alert.SourceAlertID
	stage = "restate"
	record(s, c, a, capacityResult(errorState, 95), at.Add(2*time.Minute))
	stage = "restate-confirm"
	record(s, c, a, capacityResult(errorState, 96), at.Add(3*time.Minute))
	evidence, err = s.ReadEdgeEvidence(ctx, 17, 1)
	if err != nil || evidence.Conditions[0].Alert == nil || evidence.Conditions[0].Alert.SourceAlertID != firstID ||
		evidence.Conditions[0].Alert.TransitionVersion != 2 || evidence.Conditions[0].DeliveredState != errorState {
		t.Fatalf("restate broke the incident identity: %+v %v", evidence.Conditions[0], err)
	}

	// Recovery resolves and pages once, again after two confirmed samples. A
	// restart in between keeps everything.
	reopened := certStoreReopen(t, dir)
	stage = "recovery"
	record(reopened, c, a, capacityResult(ok, 10), at.Add(4*time.Minute))
	stage = "recovery-confirm"
	record(reopened, c, a, capacityResult(ok, 9), at.Add(5*time.Minute))
	evidence, err = reopened.ReadEdgeEvidence(ctx, 17, 1)
	if err != nil || evidence.Conditions[0].Alert != nil || evidence.Conditions[0].AlertSourceID != "" || evidence.Conditions[0].DeliveredState != ok {
		t.Fatalf("recovery left an open incident: %+v %v", evidence.Conditions[0], err)
	}
	if err := reopened.db.NewRaw("SELECT COUNT(*) FROM edge_alerts WHERE subject_kind = 'capacity' AND status = 'resolved'").Scan(ctx, &intents); err != nil || intents != 1 {
		t.Fatal("capacity incident not resolved", intents, err)
	}
	if err := reopened.db.NewRaw("SELECT COUNT(*) FROM edge_delivery_outbox WHERE event_kind = 'capacity_condition'").Scan(ctx, &intents); err != nil || intents != 3 {
		t.Fatal("page history wrong", intents, err)
	}

	// A new breach opens a new identity; disabling the check closes it
	// administratively with no provider work and retires the row.
	stage = "rebreach1"
	record(reopened, c, a, capacityResult(warning, 90), at.Add(6*time.Minute))
	stage = "rebreach2"
	record(reopened, c, a, capacityResult(warning, 91), at.Add(7*time.Minute))
	evidence, err = reopened.ReadEdgeEvidence(ctx, 17, 1)
	if err != nil || evidence.Conditions[0].Alert == nil || evidence.Conditions[0].Alert.SourceAlertID == firstID {
		t.Fatalf("new breach reused a retired identity: %+v %v", evidence.Conditions[0], err)
	}
	disabled, disabledAssignment := capacityAssignment(false)
	stage = "disable"
	record(reopened, disabled, disabledAssignment, ports.CheckResult{Status: domain.StatusUp, Message: "select 1"}, at.Add(8*time.Minute))
	evidence, err = reopened.ReadEdgeEvidence(ctx, 17, 1)
	if err != nil || len(evidence.Conditions) != 0 {
		t.Fatalf("disabled check kept its row: %+v %v", evidence, err)
	}
	if err := reopened.db.NewRaw("SELECT COUNT(*) FROM edge_alerts WHERE subject_kind = 'capacity' AND status = 'firing'").Scan(ctx, &intents); err != nil || intents != 0 {
		t.Fatal("administrative close left a firing incident", intents, err)
	}
	if err := reopened.db.NewRaw("SELECT COUNT(*) FROM edge_delivery_outbox WHERE event_kind = 'capacity_condition'").Scan(ctx, &intents); err != nil || intents != 4 {
		t.Fatal("removal invented provider work", intents, err)
	}
	var transitions int
	if err := reopened.db.NewRaw("SELECT COUNT(*) FROM edge_alerts WHERE subject_kind = 'capacity'").Scan(ctx, &transitions); err != nil || transitions != 2 {
		t.Fatal("incident identities wrong", transitions, err)
	}
}
