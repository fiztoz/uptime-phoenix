package services_test

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestAccessReplayAcknowledgementRequiresIssuedOriginalIncident(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	note := "Investigating"
	prior := domain.RegionalIncident{SourceAlertID: "11111111-1111-4111-8111-111111111111", ProbeID: "probe", MonitorID: 4, AssignmentGeneration: 9, ConfigRevision: 3, Scope: domain.IncidentScopeRegional, SubjectKind: domain.IncidentSubjectAvailability, Status: domain.AlertStatusFiring, TransitionVersion: 1, StartedAt: now.Add(-time.Hour)}
	command := domain.ProbeAlertAcknowledgement{CommandID: "22222222-2222-4222-8222-222222222222", ProbeID: prior.ProbeID, SourceAlertID: prior.SourceAlertID, AssignmentGeneration: prior.AssignmentGeneration, CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), ActorDisplayName: "Operator", Note: &note}
	acked := prior
	acked.Status = domain.AlertStatusAcked
	acked.TransitionVersion = 2
	acked.AckedAt = &now
	acked.AckCommandID = command.CommandID
	acked.AckActorDisplayName = command.ActorDisplayName
	acked.AckNote = command.Note
	for _, test := range []struct {
		name   string
		mutate func(*domain.ProbeReplayAuthorityFacts, *domain.RegionalIncident)
		code   string
	}{
		{"retired assignment", func(*domain.ProbeReplayAuthorityFacts, *domain.RegionalIncident) {}, ""},
		{"unissued", func(f *domain.ProbeReplayAuthorityFacts, _ *domain.RegionalIncident) { f.IssuedAcknowledgement = nil }, "acknowledgement_unauthorized"},
		{"different actor", func(_ *domain.ProbeReplayAuthorityFacts, i *domain.RegionalIncident) {
			i.AckActorDisplayName = "Intruder"
		}, "acknowledgement_unauthorized"},
		{"different note", func(_ *domain.ProbeReplayAuthorityFacts, i *domain.RegionalIncident) { i.AckNote = nil }, "acknowledgement_unauthorized"},
		{"different command", func(_ *domain.ProbeReplayAuthorityFacts, i *domain.RegionalIncident) {
			i.AckCommandID = prior.SourceAlertID
		}, "acknowledgement_unauthorized"},
		{"different generation", func(_ *domain.ProbeReplayAuthorityFacts, i *domain.RegionalIncident) { i.AssignmentGeneration++ }, "transition_identity_conflict"},
		{"no opening", func(f *domain.ProbeReplayAuthorityFacts, _ *domain.RegionalIncident) { f.PriorIncident = nil }, "transition_version_invalid"},
		{"skipped transition", func(_ *domain.ProbeReplayAuthorityFacts, i *domain.RegionalIncident) { i.TransitionVersion++ }, "transition_identity_conflict"},
		{"at expiry", func(f *domain.ProbeReplayAuthorityFacts, _ *domain.RegionalIncident) {
			c := *f.IssuedAcknowledgement
			c.ExpiresAt = now
			f.IssuedAcknowledgement = &c
		}, "acknowledgement_unauthorized"},
		{"changed configuration", func(_ *domain.ProbeReplayAuthorityFacts, i *domain.RegionalIncident) { i.ConfigRevision++ }, "config_revision_mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := domain.ProbeReplayAuthorityFacts{ProbeID: prior.ProbeID, PriorIncident: &prior, IssuedAcknowledgement: &command}
			i := acked
			test.mutate(&f, &i)
			code, ok := (&services.AccessService{}).AuthorizeEvent(t.Context(), f, domain.ProbeReplayEvent{Seq: 2, Kind: domain.ReplayKindAlertTransition, ObservedAt: now, Incident: &i}, now)
			if code != test.code || ok != (test.code == "") {
				t.Fatalf("code=%s ok=%v", code, ok)
			}
		})
	}
	// Accepted ACK metadata survives recovery even if its source clock moves
	// before both opening and ACK. A later command cannot replace the actor.
	recovered := acked
	recovered.Status = domain.AlertStatusResolved
	recovered.TransitionVersion = 3
	rollback := now.Add(-2 * time.Hour)
	recovered.ResolvedAt = &rollback
	f := domain.ProbeReplayAuthorityFacts{ProbeID: prior.ProbeID, MonitorID: prior.MonitorID, MonitorExists: true, ConfigRevision: prior.ConfigRevision, ConfigAssignment: &domain.EdgeAssignmentIdentity{MonitorID: prior.MonitorID, Generation: prior.AssignmentGeneration, Active: true}, PriorIncident: &acked}
	for _, change := range []bool{false, true} {
		i := recovered
		if change {
			i.AckCommandID = prior.SourceAlertID
		}
		code, ok := (&services.AccessService{}).AuthorizeEvent(t.Context(), f, domain.ProbeReplayEvent{Seq: 3, Kind: domain.ReplayKindAlertTransition, ObservedAt: rollback, Incident: &i}, now)
		if ok == change || !ok && code != "acknowledgement_unauthorized" {
			t.Fatalf("recovery change=%v code=%s ok=%v", change, code, ok)
		}
	}
}
