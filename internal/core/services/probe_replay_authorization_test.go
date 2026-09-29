package services_test

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestAccessReplayAvailabilityEscalationProgress(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	due := now.Add(5 * time.Minute)
	step := int64(1)
	next := int64(2)
	opening := domain.RegionalIncident{
		SourceAlertID: "11111111-1111-4111-8111-111111111111", ProbeID: "probe", Scope: domain.IncidentScopeRegional,
		SubjectKind: domain.IncidentSubjectAvailability, MonitorID: 1, AssignmentGeneration: 2, Status: domain.AlertStatusFiring,
		TransitionVersion: 1, StartedAt: now.Add(-time.Minute), Reason: "down", ConfigRevision: 3,
		EscalationPolicyID: 30, EscalationPolicyVersion: 3, EscalationStatus: domain.EscalationStatePending,
		EscalationNextStep: &step, EscalationNextRunAt: &due,
	}
	facts := domain.ProbeReplayAuthorityFacts{ProbeID: "probe", StreamID: "stream", MonitorID: 1, MonitorExists: true, ConfigRevision: 3, ConfigAssignment: &domain.EdgeAssignmentIdentity{MonitorID: 1, Generation: 2, Active: true}, AssignmentHistory: []domain.AssignmentInterval{{ProbeID: "probe", Generation: 2, From: now.Add(-time.Hour)}}}
	a := &services.AccessService{}
	openEvent := domain.ProbeReplayEvent{Seq: 1, Kind: domain.ReplayKindAlertTransition, ObservedAt: opening.StartedAt, Incident: &opening}
	if code, ok := a.AuthorizeEvent(t.Context(), facts, openEvent, now); code != "" || !ok {
		t.Fatalf("opening ladder rejected: %s", code)
	}
	advanced := opening
	advanced.TransitionVersion = 2
	advanced.EscalationNextStep = &next
	later := now.Add(5 * time.Minute)
	advanced.EscalationNextRunAt = &later
	facts.PriorIncident = &opening
	event := domain.ProbeReplayEvent{Seq: 2, Kind: domain.ReplayKindAlertTransition, ObservedAt: now, Incident: &advanced}
	if code, ok := a.AuthorizeEvent(t.Context(), facts, event, now); code != "" || !ok {
		t.Fatalf("step advance rejected: %s", code)
	}
	stalled := advanced
	stalled.EscalationNextStep = &step
	event.Incident = &stalled
	if code, ok := a.AuthorizeEvent(t.Context(), facts, event, now); code != "transition_identity_conflict" || ok {
		t.Fatalf("unchanged rung accepted: %s", code)
	}
}

func TestAccessReplayRequiresExactConfigAndHistoricalMembership(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	a := &services.AccessService{}
	f := domain.ProbeReplayAuthorityFacts{ProbeID: "probe", StreamID: "stream", MonitorID: 1, MonitorExists: true, ConfigRevision: 3, ConfigEffectiveAt: now.Add(-time.Hour), ConfigAssignment: &domain.EdgeAssignmentIdentity{MonitorID: 1, Generation: 2, Active: true}, AssignmentHistory: []domain.AssignmentInterval{{ProbeID: "probe", Generation: 2, From: now.Add(-time.Hour), To: now}}}
	o := domain.RegionalObservation{MonitorID: 1, ProbeID: "probe", StreamID: "stream", Seq: 1, AssignmentGeneration: 2, ConfigRevision: 3, Status: domain.StatusUp, RawStatus: domain.StatusUp, ObservedAt: now.Add(-time.Minute)}
	for _, test := range []struct {
		name   string
		mutate func(*domain.RegionalObservation, *domain.ProbeReplayAuthorityFacts)
		code   string
	}{
		{"authorized history", func(*domain.RegionalObservation, *domain.ProbeReplayAuthorityFacts) {}, ""},
		{"wrong revision", func(o *domain.RegionalObservation, _ *domain.ProbeReplayAuthorityFacts) { o.ConfigRevision = 2 }, "config_revision_mismatch"},
		{"wrong generation", func(o *domain.RegionalObservation, _ *domain.ProbeReplayAuthorityFacts) { o.AssignmentGeneration = 1 }, "config_revision_mismatch"},
		{"deleted monitor", func(_ *domain.RegionalObservation, f *domain.ProbeReplayAuthorityFacts) { f.MonitorExists = false }, "monitor_not_found"},
		{"other monitor history", func(o *domain.RegionalObservation, _ *domain.ProbeReplayAuthorityFacts) { o.MonitorID = 2 }, "monitor_not_found"},
		{"exclusive interval end", func(o *domain.RegionalObservation, _ *domain.ProbeReplayAuthorityFacts) { o.ObservedAt = now }, "assignment_unauthorized"},
		{"future", func(o *domain.RegionalObservation, _ *domain.ProbeReplayAuthorityFacts) {
			o.ObservedAt = now.Add(time.Hour)
		}, "future_dated_evidence"},
		{"horizon", func(o *domain.RegionalObservation, _ *domain.ProbeReplayAuthorityFacts) {
			o.ObservedAt = now.Add(-8 * 24 * time.Hour)
		}, "history_horizon_exceeded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			obs, facts := o, f
			test.mutate(&obs, &facts)
			e := domain.ProbeReplayEvent{Seq: 1, Kind: domain.ReplayKindObservation, ObservedAt: obs.ObservedAt, Observation: &obs}
			code, ok := a.AuthorizeEvent(t.Context(), facts, e, now)
			if code != test.code || ok != (test.code == "") {
				t.Fatalf("code=%s ok=%v", code, ok)
			}
		})
	}
}

func TestAccessReplayDeliveryUsesExactAcceptedParentAndLinkedChannel(t *testing.T) {
	now := time.Now().UTC()
	// The store always labels a parent transition with its scope and subject kind
	// (probe_replay.go), so the fixture does too: an unlabeled parent must fail closed.
	parent := domain.RegionalIncident{SourceAlertID: "11111111-1111-4111-8111-111111111111", ProbeID: "probe", Scope: domain.IncidentScopeRegional, SubjectKind: domain.IncidentSubjectAvailability, MonitorID: 1, AssignmentGeneration: 2, TransitionVersion: 3, ConfigRevision: 9, StartedAt: now.Add(-time.Hour)}
	f := domain.ProbeReplayAuthorityFacts{ProbeID: "probe", MonitorID: 1, MonitorExists: true, ConfigRevision: 9, ConfigAssignment: &domain.EdgeAssignmentIdentity{MonitorID: 1, Generation: 2, Active: true}, ParentTransition: &parent, Channels: map[int64]int64{7: 4}}
	d := domain.RegionalDelivery{DeliveryID: "22222222-2222-4222-8222-222222222222", SourceAlertID: parent.SourceAlertID, SourceTransitionVersion: 3, ProbeID: "probe", NotificationID: 7, NotificationVersion: 4, EventKind: domain.DeliveryEventStatusChange, Attempt: 1, Status: domain.DeliveryStatusSent, ObservedAt: now}
	for _, test := range []struct {
		name   string
		mutate func(*domain.RegionalDelivery, *domain.ProbeReplayAuthorityFacts)
		code   string
	}{
		{"valid", func(*domain.RegionalDelivery, *domain.ProbeReplayAuthorityFacts) {}, ""},
		{"not merely below latest", func(d *domain.RegionalDelivery, _ *domain.ProbeReplayAuthorityFacts) { d.SourceTransitionVersion = 2 }, "delivery_parent_not_found"},
		{"unlinked channel", func(d *domain.RegionalDelivery, _ *domain.ProbeReplayAuthorityFacts) { d.NotificationID = 8 }, "channel_unauthorized"},
		{"wrong channel version below config", func(d *domain.RegionalDelivery, _ *domain.ProbeReplayAuthorityFacts) { d.NotificationVersion = 3 }, "channel_unauthorized"},
		{"unlabeled parent fails closed", func(_ *domain.RegionalDelivery, f *domain.ProbeReplayAuthorityFacts) {
			unlabeled := parent
			unlabeled.SubjectKind, unlabeled.Scope = "", ""
			f.ParentTransition = &unlabeled
		}, "event_invalid"},
		{"certificate outcome on availability parent", func(d *domain.RegionalDelivery, _ *domain.ProbeReplayAuthorityFacts) {
			d.EventKind = domain.DeliveryEventCertificateExpiry
		}, "event_invalid"},
		{"hub intent collision", func(_ *domain.RegionalDelivery, f *domain.ProbeReplayAuthorityFacts) { f.DeliveryIntentExists = true }, "delivery_identity_conflict"},
		{"terminal regression", func(d *domain.RegionalDelivery, f *domain.ProbeReplayAuthorityFacts) {
			prior := *d
			f.PriorDelivery = &prior
			d.Status = domain.DeliveryStatusRetrying
			d.Attempt++
		}, "delivery_outcome_conflict"},
	} {
		t.Run(test.name, func(t *testing.T) {
			delivery, facts := d, f
			test.mutate(&delivery, &facts)
			code, ok := (&services.AccessService{}).AuthorizeEvent(t.Context(), facts, domain.ProbeReplayEvent{Seq: 1, Kind: domain.ReplayKindDeliveryResult, ObservedAt: now, Delivery: &delivery}, now)
			if code != test.code || ok != (test.code == "") {
				t.Fatalf("code=%s ok=%v", code, ok)
			}
		})
	}
}

func TestMaxFutureClockSkewIsOperatorBound(t *testing.T) {
	if services.MaxFutureClockSkew != 30*time.Second {
		t.Fatalf("future-evidence bound = %s; docs/multi-region/M6_OPERATOR_REQUIREMENTS.md says 30s", services.MaxFutureClockSkew)
	}
}
