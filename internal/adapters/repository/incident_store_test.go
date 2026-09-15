package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	incidentLocalID  = "7c73a9c5-77ad-4dc0-926d-123bbab94540"
	incidentRemoteID = "8d84b0d6-88be-4ed1-a37e-234ccbc05651"
	deliveryLocalID  = "9e95c1e7-99cf-5fe2-b48f-345ddcd16762"
	ackCommandID     = "ae06d2f8-00d0-4af3-c590-456eedd27873"
)

func TestRegionalIncidentsAreIndependentPerProbe(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			monitorID := f.monitor(t)
			if _, err := f.assignments.InitializeLocal(ctx, monitorID); err != nil {
				t.Fatal(err)
			}
			remote := f.remote(t, probeRegistryID1, "asia")
			if _, err := f.assignments.Replace(ctx, monitorID, 1, []string{"local", remote.ID}, domain.HealthPolicyAnyDown); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			localObs := regionalSample(monitorID, "local", localStreamID, 1, 1, domain.StatusDown, 1, now)
			localInc := availabilityIncident(incidentLocalID, monitorID, "local", now)
			if err := f.commits.Commit(ctx, domain.RegionalCommit{
				Observation: localObs, State: stateFrom(localObs), Incident: localInc,
			}); err != nil {
				t.Fatal(err)
			}
			if localInc.HubIncidentID == 0 {
				t.Fatal("hub mirror id was not assigned")
			}
			remoteObs := regionalSample(monitorID, remote.ID, remoteStreamID, 1, 1, domain.StatusUp, 0, now)
			remoteInc := availabilityIncident(incidentRemoteID, monitorID, remote.ID, now)
			if err := f.commits.Commit(ctx, domain.RegionalCommit{
				Observation: remoteObs, State: stateFrom(remoteObs), Incident: remoteInc,
			}); err != nil {
				t.Fatal(err)
			}
			listed, err := f.incidents.ListIncidentsByMonitor(ctx, monitorID)
			if err != nil || len(listed) != 2 {
				t.Fatalf("list: %d %v", len(listed), err)
			}
			otherID := f.monitor(t)
			if leaked, err := f.incidents.ListIncidentsByMonitor(ctx, otherID); err != nil || len(leaked) != 0 {
				t.Fatalf("leaked incidents: %+v %v", leaked, err)
			}

			again := availabilityIncident(incidentLocalID, monitorID, "local", now)
			if err := f.incidents.PutIncident(ctx, again); err != nil || again.HubIncidentID != localInc.HubIncidentID {
				t.Fatalf("idempotent put: %+v %v", again, err)
			}
			ackedAt := now.Add(time.Minute)
			ack := availabilityIncident(incidentLocalID, monitorID, "local", now)
			ack.TransitionVersion = 2
			ack.Status = domain.AlertStatusAcked
			ack.AckedAt = &ackedAt
			ack.AckCommandID = ackCommandID
			ack.AckActorDisplayName = "ops"
			if err := f.incidents.PutIncident(ctx, ack); err != nil {
				t.Fatal(err)
			}
			got, err := f.incidents.GetIncident(ctx, incidentLocalID)
			if err != nil || got.Status != domain.AlertStatusAcked || got.HubIncidentID != localInc.HubIncidentID {
				t.Fatalf("acked: %+v %v", got, err)
			}
			stale := availabilityIncident(incidentLocalID, monitorID, "local", now)
			stale.TransitionVersion = 1
			if err := f.incidents.PutIncident(ctx, stale); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("lower version: %v", err)
			}
			mutated := availabilityIncident(incidentLocalID, monitorID, "local", now.Add(time.Hour))
			mutated.TransitionVersion = 3
			if err := f.incidents.PutIncident(ctx, mutated); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("immutable start: %v", err)
			}
			aggregate := availabilityIncident("bf17e3a9-11e1-4b04-d6a1-567ffee38984", monitorID, "local", now)
			aggregate.Scope = domain.IncidentScopeAggregate
			if err := f.incidents.PutIncident(ctx, aggregate); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("aggregate: %v", err)
			}

			delivery := &domain.RegionalDelivery{
				DeliveryID: deliveryLocalID, SourceAlertID: incidentLocalID, SourceTransitionVersion: 2,
				ProbeID: "local", NotificationID: 3, NotificationVersion: 1,
				EventKind: domain.DeliveryEventStatusChange, Attempt: 1,
				Status: domain.DeliveryStatusSent, ObservedAt: now.In(time.FixedZone("UTC+7", 7*3600)),
			}
			if err := f.deliveries.PutDelivery(ctx, delivery); err != nil {
				t.Fatal(err)
			}
			stored, err := f.deliveries.GetDelivery(ctx, deliveryLocalID)
			if err != nil || !stored.ObservedAt.Equal(now.UTC()) || stored.Attempt != 1 {
				t.Fatalf("delivery utc: %+v %v", stored, err)
			}
			delivery.Attempt = 2
			delivery.Status = domain.DeliveryStatusFailed
			delivery.ErrorCode = "provider_unavailable"
			if err := f.deliveries.PutDelivery(ctx, delivery); err != nil {
				t.Fatal(err)
			}
			updated, err := f.deliveries.GetDelivery(ctx, deliveryLocalID)
			if err != nil || updated.Attempt != 2 || updated.Status != domain.DeliveryStatusFailed {
				t.Fatalf("delivery attempt: %+v %v", updated, err)
			}
			orphan := *delivery
			orphan.DeliveryID = "c028f4b0-22f2-4c15-e7b2-678000f49095"
			orphan.SourceAlertID = "d13905c1-33a3-4d26-f8c3-789111a5a1a6"
			if err := f.deliveries.PutDelivery(ctx, &orphan); !errors.Is(err, ports.ErrNotFound) && !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("unknown incident delivery: %v", err)
			}

			badObs := regionalSample(monitorID, "local", localStreamID, 1, 2, domain.StatusDown, 2, now.Add(time.Second))
			badInc := availabilityIncident("e24a16d2-44b4-4e37-a9d4-890222b6b2b7", monitorID, "local", now)
			badInc.Status = "nope"
			if err := f.commits.Commit(ctx, domain.RegionalCommit{
				Observation: badObs, State: stateFrom(badObs), Incident: badInc,
			}); err == nil {
				t.Fatal("invalid incident committed")
			}
			rows, err := f.commits.ListObservations(ctx, monitorID, "local", now.Add(-time.Minute), now.Add(time.Minute))
			if err != nil || len(rows) != 1 {
				t.Fatalf("observation rolled back: %d %v", len(rows), err)
			}

			if err := runEngineMigration(t, f.db, f.engine, "038_probe_incidents", "down"); err == nil {
				t.Fatal("downgrade discarded incidents")
			}
			if _, err := f.db.ExecContext(ctx, "DELETE FROM probe_delivery_events"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.ExecContext(ctx, "DELETE FROM probe_incidents"); err != nil {
				t.Fatal(err)
			}
			if err := runEngineMigration(t, f.db, f.engine, "038_probe_incidents", "down"); err != nil {
				t.Fatalf("empty down: %v", err)
			}
			if err := runEngineMigration(t, f.db, f.engine, "038_probe_incidents", "up"); err != nil {
				t.Fatalf("restore 038: %v", err)
			}
		})
	}
}

func availabilityIncident(id string, monitorID int64, probeID string, started time.Time) *domain.RegionalIncident {
	return &domain.RegionalIncident{
		SourceAlertID: id, Scope: domain.IncidentScopeRegional, MonitorID: monitorID, ProbeID: probeID,
		AssignmentGeneration: 1, Status: domain.AlertStatusFiring, TransitionVersion: 1,
		StartedAt: started, ConfigRevision: 1, SubjectKind: domain.IncidentSubjectAvailability,
	}
}
