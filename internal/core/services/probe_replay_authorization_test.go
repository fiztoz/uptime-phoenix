package services_test

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

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
	parent := domain.RegionalIncident{SourceAlertID: "11111111-1111-4111-8111-111111111111", ProbeID: "probe", MonitorID: 1, AssignmentGeneration: 2, TransitionVersion: 3, ConfigRevision: 9, StartedAt: now.Add(-time.Hour)}
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
