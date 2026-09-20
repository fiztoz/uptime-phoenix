package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type edgeSourceFake struct {
	i       domain.EdgeIdentity
	e       domain.EdgeMonitorEvidence
	records []domain.EdgeCheckRecord
	fail    error
}

func (f *edgeSourceFake) ReadIdentity(context.Context) (domain.EdgeIdentity, error) { return f.i, nil }
func (*edgeSourceFake) AcceptConnectionGeneration(context.Context, string, int64) error {
	return errors.New("unexpected session mutation")
}
func (f *edgeSourceFake) ReadEdgeEvidence(context.Context, int64, int64) (domain.EdgeMonitorEvidence, error) {
	return f.e, nil
}
func (f *edgeSourceFake) CommitEdgeCheck(_ context.Context, r domain.EdgeCheckRecord) (domain.RegionalObservation, error) {
	if f.fail != nil {
		return domain.RegionalObservation{}, f.fail
	}
	f.records = append(f.records, r)
	o := r.Observation
	o.Seq = int64(len(f.records))
	f.e.State = &domain.RegionalState{Seq: o.Seq, Status: o.Status, DownCount: o.DownCount}
	if r.Incident != nil {
		f.e.Incident = r.Incident
	}
	if len(r.DeliveryIntents) > 0 {
		at := o.ObservedAt
		f.e.LastEnqueuedAt = &at
	}
	return o, nil
}
func edgeServiceFixture() (*edgeSourceFake, *domain.EdgeResolvedConfig, domain.EdgeResolvedAssignment) {
	f := &edgeSourceFake{i: domain.EdgeIdentity{HubID: "hub", ProbeID: "probe", StreamID: "stream", ConfigRevision: 3}}
	a := domain.EdgeResolvedAssignment{Monitor: &domain.Monitor{ID: 1, Active: true, Type: "http", MaxRetries: 1, ResendInterval: 1}, Generation: 2, NotificationLinks: []domain.MonitorNotification{{MonitorID: 1, NotificationID: 7}}}
	c := &domain.EdgeResolvedConfig{Metadata: domain.ProbeConfigMetadata{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: "hub", ProbeID: "probe"}, Revision: 3}, Assignments: []domain.EdgeResolvedAssignment{a}, Channels: map[int64]domain.EdgeResolvedChannel{7: {Notification: &domain.Notification{ID: 7, Active: true}, Version: 3}}, Maintenance: map[int64]*domain.MaintenanceWindow{}}
	return f, c, a
}
func TestEdgeRecordingRetryIncidentResendAndRecovery(t *testing.T) {
	f, c, a := edgeServiceFixture()
	svc := NewEdgeRecordingService(f, f, nil)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.FixedZone("local", 7*3600))
	record := func(status domain.Status, after time.Duration) domain.RegionalObservation {
		t.Helper()
		o, err := svc.Record(t.Context(), c, a, ports.CheckResult{Status: status, Message: "safe diagnostic"}, at.Add(after))
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	if o := record(domain.StatusDown, 0); o.Status != domain.StatusPending || len(f.records[0].DeliveryIntents) != 0 || f.e.Incident != nil {
		t.Fatal("unconfirmed failure paged")
	}
	if o := record(domain.StatusDown, time.Second); o.Status != domain.StatusDown || len(f.records[1].DeliveryIntents) != 1 || f.e.Incident == nil {
		t.Fatal("confirmed failure lost work")
	}
	id := f.e.Incident.SourceAlertID
	record(domain.StatusDown, 30*time.Second)
	if len(f.records[2].DeliveryIntents) != 0 || f.records[2].Incident != nil {
		t.Fatal("resend bypassed throttle")
	}
	record(domain.StatusDown, 61*time.Second)
	if len(f.records[3].DeliveryIntents) != 1 || f.records[3].Incident != nil || f.records[3].DeliveryIntents[0].SourceAlertID != id {
		t.Fatal("resend changed lifecycle")
	}
	record(domain.StatusUp, 62*time.Second)
	if len(f.records[4].DeliveryIntents) != 1 || f.e.Incident.SourceAlertID != id || f.e.Incident.Status != domain.AlertStatusResolved || f.e.Incident.TransitionVersion != 2 {
		t.Fatal("recovery lost lifecycle")
	}
	for _, r := range f.records {
		if r.Observation.ObservedAt.Location() != time.UTC || r.Observation.ReceivedAt.Location() != time.UTC {
			t.Fatal("non UTC source boundary")
		}
	}
	record(domain.StatusUp, 63*time.Second)
	if len(f.records[5].DeliveryIntents) != 0 || f.records[5].Incident != nil {
		t.Fatal("duplicate recovery")
	}
}
func TestEdgeRecordingMaintenanceAndAuthority(t *testing.T) {
	f, c, a := edgeServiceFixture()
	svc := NewEdgeRecordingService(f, f, nil)
	at := time.Now().UTC()
	a.MaintenanceIDs = []int64{9}
	c.Maintenance[9] = &domain.MaintenanceWindow{ID: 9, Active: true, Strategy: "single", StartDate: at.Add(-time.Minute), EndDate: at.Add(time.Minute)}
	o, err := svc.Record(t.Context(), c, a, ports.CheckResult{Status: domain.StatusDown}, at)
	if err != nil || o.Status != domain.StatusMaintenance || f.e.Incident != nil || len(f.records[0].DeliveryIntents) != 0 {
		t.Fatalf("maintenance paged: %+v %v", o, err)
	}
	f.i.ConfigRevision++
	if _, err := svc.Record(t.Context(), c, a, ports.CheckResult{Status: domain.StatusDown}, at); !errors.Is(err, ports.ErrConflict) || len(f.records) != 1 {
		t.Fatal("stale config wrote")
	}
	f.i.ConfigRevision--
	f.fail = ports.ErrStaleLocalState
	if _, err := svc.Record(t.Context(), c, a, ports.CheckResult{Status: domain.StatusDown}, at); !errors.Is(err, ports.ErrStaleLocalState) {
		t.Fatal("exhausted CAS reported success")
	}
	c.Maintenance[9].Strategy = "cron"
	if _, err := EdgeMaintenanceActive(c, a, nil, at); err == nil {
		t.Fatal("missing cron evaluator silently skipped maintenance")
	}
}
