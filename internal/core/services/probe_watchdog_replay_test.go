package services_test

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestWatchdogReplayLifecycleAuthority(t *testing.T) {
	now := time.Now().UTC()
	base := domain.RegionalIncident{SourceAlertID: "11111111-1111-4111-8111-111111111111", ProbeID: "22222222-2222-4222-8222-222222222222", Scope: domain.IncidentScopeProbeConnection, SubjectKind: domain.IncidentSubjectWatchdog, Status: domain.AlertStatusFiring, TransitionVersion: 1, ConfigRevision: 2, StartedAt: now}
	for _, name := range []string{"opening", "disabled", "missing config", "hub owner", "wrong scope", "monitor", "unknown ACK", "rollback recovery", "administrative disable", "changed start", "terminal", "missing opening"} {
		t.Run(name, func(t *testing.T) {
			i, prior := base, base
			facts := domain.ProbeReplayAuthorityFacts{ProbeID: base.ProbeID, ConfigFound: true, ConfigRevision: 2, WatchdogEnabled: true}
			code := ""
			switch name {
			case "disabled":
				facts.WatchdogEnabled, code = false, "watchdog_disabled"
			case "missing config":
				facts.ConfigFound, code = false, "config_revision_mismatch"
			case "hub owner":
				facts.HubOwnedIncident, code = true, "source_owner_conflict"
			case "wrong scope":
				i.Scope, code = domain.IncidentScopeRegional, "transition_identity_conflict"
			case "monitor":
				i.MonitorID, code = 1, "transition_identity_conflict"
			case "unknown ACK":
				i.Status, i.TransitionVersion, i.AckedAt, i.AckCommandID, i.AckActorDisplayName = domain.AlertStatusAcked, 2, &now, base.SourceAlertID, "Forged actor"
				facts.PriorIncident, code = &prior, "acknowledgement_unauthorized"
			case "rollback recovery", "administrative disable", "changed start", "terminal", "missing opening":
				at := now.Add(-time.Minute)
				i.Status, i.TransitionVersion, i.ResolvedAt = domain.AlertStatusResolved, 2, &at
				facts.PriorIncident = &prior
				switch name {
				case "administrative disable":
					facts.WatchdogEnabled = false
				case "changed start":
					i.StartedAt, code = at, "transition_identity_conflict"
				case "terminal":
					prior.Status, prior.ResolvedAt, code = domain.AlertStatusResolved, &at, "transition_identity_conflict"
				case "missing opening":
					facts.PriorIncident, code = nil, "transition_identity_conflict"
				}
			}
			e := domain.ProbeReplayEvent{Seq: 3, Kind: domain.ReplayKindWatchdogTransition, ObservedAt: now.Add(-time.Minute), Incident: &i}
			got, ok := (&services.AccessService{}).AuthorizeEvent(t.Context(), facts, e, now)
			if got != code || ok != (code == "") {
				t.Fatalf("code=%q accepted=%v; want %q", got, ok, code)
			}
		})
	}
}

func TestWatchdogReplayDeliveryUsesResendConfigAndSourceOrder(t *testing.T) {
	now := time.Now().UTC()
	parent := domain.RegionalIncident{SourceAlertID: "11111111-1111-4111-8111-111111111111", ProbeID: "22222222-2222-4222-8222-222222222222", Scope: domain.IncidentScopeProbeConnection, SubjectKind: domain.IncidentSubjectWatchdog, Status: domain.AlertStatusFiring, TransitionVersion: 1, ConfigRevision: 2, StartedAt: now}
	for _, name := range []string{"resend", "rollback retry", "wrong channel", "missing config", "disabled", "missing parent", "wrong subject", "hub owner", "older config", "terminal result"} {
		t.Run(name, func(t *testing.T) {
			p := parent
			d := domain.RegionalDelivery{DeliveryID: "33333333-3333-4333-8333-333333333333", ProbeID: p.ProbeID, SourceAlertID: p.SourceAlertID, SourceTransitionVersion: 1, NotificationID: 7, NotificationVersion: 3, EventKind: domain.DeliveryEventProbeConnection, Attempt: 2, Status: domain.DeliveryStatusSent, ObservedAt: now.Add(-time.Minute)}
			f := domain.ProbeReplayAuthorityFacts{ProbeID: p.ProbeID, ConfigFound: true, ConfigRevision: 3, WatchdogEnabled: true, ParentTransition: &p, Channels: map[int64]int64{7: 3}}
			code := ""
			switch name {
			case "rollback retry", "terminal result":
				previous := d
				previous.Attempt, previous.ObservedAt, previous.Status = 1, now, domain.DeliveryStatusRetrying
				f.PriorDelivery = &previous
				if name == "terminal result" {
					previous.Status, code = domain.DeliveryStatusSent, "delivery_outcome_conflict"
				}
			case "wrong channel":
				d.NotificationID, code = 8, "channel_unauthorized"
			case "missing config":
				f.ConfigFound, code = false, "config_revision_mismatch"
			case "disabled":
				f.WatchdogEnabled, code = false, "config_revision_mismatch"
			case "missing parent":
				f.ParentTransition, code = nil, "delivery_parent_not_found"
			case "wrong subject":
				p.SubjectKind, code = domain.IncidentSubjectAvailability, "event_invalid"
			case "hub owner":
				f.HubOwnedIncident, code = true, "source_owner_conflict"
			case "older config":
				d.NotificationVersion, f.ConfigRevision, f.Channels[7], code = 1, 1, 1, "config_revision_mismatch"
			}
			got, ok := (&services.AccessService{}).AuthorizeEvent(t.Context(), f, domain.ProbeReplayEvent{Seq: 5, Kind: domain.ReplayKindDeliveryResult, ObservedAt: d.ObservedAt, Delivery: &d}, now)
			if got != code || ok != (code == "") {
				t.Fatalf("code=%q accepted=%v; want %q", got, ok, code)
			}
		})
	}
}
