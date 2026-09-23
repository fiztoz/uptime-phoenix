package repository_test

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func capacityIncidentEvent(r replayFixture, seq int64, id, kind string, status string, version int64) domain.ProbeReplayEvent {
	incident := domain.RegionalIncident{
		SourceAlertID: id, Scope: domain.IncidentScopeRegional, SubjectKind: domain.IncidentSubjectCapacity,
		ConditionKind: kind, MonitorID: r.monitor, ProbeID: r.session.ProbeID, AssignmentGeneration: 1,
		Status: status, TransitionVersion: version, StartedAt: r.at.Add(-time.Second),
		Reason: "storage 84.0% exceeds threshold 80%", ConfigRevision: 1,
	}
	if status == domain.AlertStatusResolved {
		at := r.at
		incident.ResolvedAt = &at
		incident.Reason = "recovered to 10.0%"
	}
	return domain.ProbeReplayEvent{Seq: seq, Kind: domain.ReplayKindAlertTransition, ObservedAt: r.at, Incident: &incident}
}

func capacityDeliveryEvent(r replayFixture, seq int64, id, eventKind string, version int64, status string) domain.ProbeReplayEvent {
	deliveryID := [3]string{"77777777-7777-4777-8777-777777777777", "88888888-8888-4888-8888-888888888888", "99999999-9999-4999-8999-999999999999"}
	delivery := domain.RegionalDelivery{
		DeliveryID: deliveryID[version-1], SourceAlertID: id, SourceTransitionVersion: version,
		ProbeID: r.session.ProbeID, NotificationID: r.channel, NotificationVersion: 1,
		EventKind: eventKind, Attempt: 1, Status: status, ObservedAt: r.at,
	}
	return domain.ProbeReplayEvent{Seq: seq, Kind: domain.ReplayKindDeliveryResult, ObservedAt: r.at, Delivery: &delivery}
}

func TestProbeCapacityPagingAcceptance(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("IncidentLifecycleMirrorsWithoutProviderWork", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				id := "66666666-6666-4666-8666-666666666666"
				got := r.ingest(t, r.batch(
					capacityIncidentEvent(r, 1, id, domain.MonitorConditionStorage, domain.AlertStatusFiring, 1),
					capacityDeliveryEvent(r, 2, id, domain.DeliveryEventCapacityCondition, 1, domain.DeliveryStatusSent),
					capacityIncidentEvent(r, 3, id, domain.MonitorConditionStorage, domain.AlertStatusFiring, 2),
					capacityDeliveryEvent(r, 4, id, domain.DeliveryEventCapacityCondition, 2, domain.DeliveryStatusSent),
					capacityIncidentEvent(r, 5, id, domain.MonitorConditionStorage, domain.AlertStatusResolved, 3),
					capacityDeliveryEvent(r, 6, id, domain.DeliveryEventCapacityCondition, 3, domain.DeliveryStatusSent),
				))
				if got.AcceptedCount != 6 || len(got.Rejected) != 0 {
					t.Fatal("capacity lifecycle rejected", got)
				}
				if n := replayCount(t, r.f, "probe_incidents"); n != 1 {
					t.Fatal("state changes invented incident identities", n)
				}
				if n := replayCount(t, r.f, "probe_delivery_events"); n != 3 {
					t.Fatal("delivery outcomes lost", n)
				}
				for _, table := range []string{"probe_delivery_intents", "alerts", "alert_escalations"} {
					if replayCount(t, r.f, table) != 0 {
						t.Fatal("mirrored capacity evidence created hub provider work", table)
					}
				}
				// A duplicate receipt adds nothing.
				again := r.ingest(t, r.batch(
					capacityIncidentEvent(r, 1, id, domain.MonitorConditionStorage, domain.AlertStatusFiring, 1),
					capacityDeliveryEvent(r, 2, id, domain.DeliveryEventCapacityCondition, 1, domain.DeliveryStatusSent),
					capacityIncidentEvent(r, 3, id, domain.MonitorConditionStorage, domain.AlertStatusFiring, 2),
					capacityDeliveryEvent(r, 4, id, domain.DeliveryEventCapacityCondition, 2, domain.DeliveryStatusSent),
					capacityIncidentEvent(r, 5, id, domain.MonitorConditionStorage, domain.AlertStatusResolved, 3),
					capacityDeliveryEvent(r, 6, id, domain.DeliveryEventCapacityCondition, 3, domain.DeliveryStatusSent),
				))
				if again.DuplicateCount != 6 || replayCount(t, r.f, "probe_incidents") != 1 {
					t.Fatal("duplicate receipt rewrote the mirror", again)
				}
			})

			t.Run("CapacityRulesAreEnforced", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				id := "66666666-6666-4666-8666-666666666666"
				// Nothing may follow the single resolution.
				r.ingest(t, r.batch(
					capacityIncidentEvent(r, 1, id, domain.MonitorConditionStorage, domain.AlertStatusFiring, 1),
					capacityIncidentEvent(r, 2, id, domain.MonitorConditionStorage, domain.AlertStatusResolved, 2),
				))
				got := r.ingest(t, r.batch(capacityIncidentEvent(r, 3, id, domain.MonitorConditionStorage, domain.AlertStatusFiring, 3)))
				if len(got.Rejected) != 1 || got.Rejected[0].Code != "transition_identity_conflict" {
					t.Fatal("post-resolution transition accepted", got)
				}
				// A capacity outcome can never authorize an availability send, and an
				// availability outcome cannot ride a capacity incident.
				r.ingest(t, r.batch(
					capacityIncidentEvent(r, 4, "55555555-5555-4555-8555-555555555555", domain.MonitorConditionStorage, domain.AlertStatusFiring, 1),
				))
				wrongKind := r.ingest(t, r.batch(capacityDeliveryEvent(r, 5, "55555555-5555-4555-8555-555555555555", domain.DeliveryEventStatusChange, 1, domain.DeliveryStatusSent)))
				if len(wrongKind.Rejected) != 1 || wrongKind.Rejected[0].Code != "event_invalid" {
					t.Fatal("delivery kind escaped its incident subject", wrongKind)
				}
				// Acknowledgement belongs to availability incidents only.
				acked := capacityIncidentEvent(r, 6, "44444444-4444-4444-8444-444444444444", domain.MonitorConditionStorage, domain.AlertStatusAcked, 2)
				got = r.ingest(t, r.batch(acked))
				if len(got.Rejected) != 1 || got.Rejected[0].Code != "event_invalid" {
					t.Fatal("capacity incident acknowledged remotely", got)
				}
				// A capacity transition may name exactly its own incident, but only
				// after its raw evidence: raw samples always precede promotion.
				r.ingest(t, r.batch(capacityObservationEvent(r, 7, capacityEvidenceSample(domain.MonitorConditionStorage, domain.ConditionStateWarning, 84, r.at))))
				correlated := capacityTransitionEvent(r, 8, domain.MonitorConditionStorage, nil, domain.ConditionStateWarning)
				incident := "55555555-5555-4555-8555-555555555555"
				correlated.Condition.SourceAlertID = &incident
				if got := r.ingest(t, r.batch(correlated)); len(got.Rejected) != 0 || got.AcceptedCount != 1 {
					t.Fatal("correlated condition transition rejected", got)
				}
			})
		})
	}
}
