package edge

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func checkRecord() domain.EdgeCheckRecord {
	i := testIdentity()
	at := time.Now().UTC().Truncate(time.Microsecond)
	incident := &domain.RegionalIncident{SourceAlertID: "d9544f89-b7b8-426a-bf9c-2658fa8d09b5", Scope: domain.IncidentScopeRegional, SubjectKind: domain.IncidentSubjectAvailability, MonitorID: 17, ProbeID: i.ProbeID, AssignmentGeneration: 1, Status: domain.AlertStatusFiring, TransitionVersion: 1, StartedAt: at, Reason: "connection refused", ConfigRevision: 1}
	return domain.EdgeCheckRecord{Observation: domain.RegionalObservation{MonitorID: 17, ProbeID: i.ProbeID, StreamID: i.StreamID, AssignmentGeneration: 1, ConfigRevision: 1, Status: domain.StatusDown, RawStatus: domain.StatusDown, DownCount: 1, Important: true, Message: "connection refused", ObservedAt: at, ReceivedAt: at}, Incident: incident, DeliveryIntents: []domain.DeliveryIntent{{DeliveryID: "65f61ca6-9802-48b9-9189-377e7c6ec25b", SourceAlertID: incident.SourceAlertID, SourceTransitionVersion: 1, ProbeID: i.ProbeID, NotificationID: 10, NotificationVersion: 1, EventKind: domain.DeliveryEventStatusChange, AvailableAt: at}}}
}

func TestEdgeCheckAtomicRecordingAndRestart(t *testing.T) {
	s, dir := testStore(t)
	s.telemetry = probe.EdgeTelemetryEncoder{}
	enroll(t, s)
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 1)); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, table := range []string{"edge_telemetry_outbox", "edge_alerts", "edge_delivery_outbox", "edge_regional_state"} {
		if _, err := s.db.ExecContext(ctx, "CREATE TRIGGER fail_record BEFORE INSERT ON "+table+" BEGIN SELECT RAISE(ABORT, 'injected'); END"); err != nil {
			t.Fatal(err)
		}
		if got, err := s.CommitEdgeCheck(ctx, checkRecord()); err == nil || got.Seq != 0 {
			t.Fatalf("partial commit with fault on %s: %+v %v", table, got, err)
		}
		if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_record"); err != nil {
			t.Fatal(err)
		}
		i, err := s.ReadIdentity(ctx)
		if err != nil || i.LastCreatedSeq != 0 {
			t.Fatalf("sequence escaped rollback: %+v %v", i, err)
		}
		for _, name := range []string{"edge_telemetry_outbox", "edge_alerts", "edge_delivery_outbox", "edge_regional_state"} {
			var n int
			if err := s.db.NewRaw("SELECT COUNT(*) FROM "+name).Scan(ctx, &n); err != nil || n != 0 {
				t.Fatalf("%s escaped rollback: %d %v", name, n, err)
			}
		}
	}
	down := checkRecord()
	got, err := s.CommitEdgeCheck(ctx, down)
	if err != nil || got.Seq != 1 {
		t.Fatalf("DOWN commit: %+v %v", got, err)
	}
	i, err := s.ReadIdentity(ctx)
	if err != nil || i.LastCreatedSeq != 2 {
		t.Fatalf("observation+incident not sequenced: %+v %v", i, err)
	}
	if _, err := s.CommitEdgeCheck(ctx, down); !errors.Is(err, ports.ErrStaleLocalState) {
		t.Fatalf("duplicate commit accepted: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dir, testIdentity(), WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	evidence, err := reopened.ReadEdgeEvidence(ctx, 17, 1)
	if err != nil || evidence.State == nil || evidence.State.Seq != 1 || evidence.Incident == nil || evidence.Incident.SourceAlertID != down.Incident.SourceAlertID || evidence.LastEnqueuedAt == nil {
		t.Fatalf("restart lost evidence: %+v %v", evidence, err)
	}
	// A subsequent recovery advances the same stream beyond both previous events.
	up := checkRecord()
	up.ExpectedStateSeq = 1
	up.ExpectedIncidentVersion = 1
	up.Observation.Status, up.Observation.RawStatus, up.Observation.DownCount = domain.StatusUp, domain.StatusUp, 0
	up.Incident = evidence.Incident
	resolvedAt := up.Observation.ObservedAt
	up.Incident.Status, up.Incident.TransitionVersion, up.Incident.ResolvedAt = domain.AlertStatusResolved, 2, &resolvedAt
	up.DeliveryIntents[0].DeliveryID = "01b011be-179c-4f09-9b6d-de21655c7a46"
	up.DeliveryIntents[0].SourceTransitionVersion = 2
	up.DeliveryIntents[0].EventKind = domain.DeliveryEventIncidentSummary
	got, err = reopened.CommitEdgeCheck(ctx, up)
	if err != nil || got.Seq != 3 {
		t.Fatalf("UP commit reused sequence: %+v %v", got, err)
	}
	i, err = reopened.ReadIdentity(ctx)
	if err != nil || i.LastCreatedSeq != 4 {
		t.Fatalf("recovery sequence: %+v %v", i, err)
	}
	var eventSeqs []int64
	if err := reopened.db.NewRaw("SELECT seq FROM edge_telemetry_outbox ORDER BY seq").Scan(ctx, &eventSeqs); err != nil || len(eventSeqs) != 4 {
		t.Fatalf("durable telemetry: %v %v", eventSeqs, err)
	}
	for n, seq := range eventSeqs {
		if seq != int64(n+1) {
			t.Fatal("telemetry sequence gap")
		}
	}
	// Deleting retained history must never reset the source counter.
	if _, err := reopened.db.ExecContext(ctx, "DELETE FROM edge_telemetry_outbox"); err != nil {
		t.Fatal(err)
	}
	up.ExpectedStateSeq, up.Incident, up.DeliveryIntents = 3, nil, nil
	up.ExpectedIncidentVersion = 2
	up.Observation.Important = false
	got, err = reopened.CommitEdgeCheck(ctx, up)
	if err != nil || got.Seq != 5 {
		t.Fatalf("history cleanup reset sequence: %+v %v", got, err)
	}
}

func TestEdgeCheckRejectsObsoleteAuthorityAndOverflow(t *testing.T) {
	s, _ := testStore(t)
	s.telemetry = probe.EdgeTelemetryEncoder{}
	enroll(t, s)
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 1)); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*domain.EdgeCheckRecord){
		"foreign probe":    func(r *domain.EdgeCheckRecord) { r.Observation.ProbeID = testHubID },
		"foreign stream":   func(r *domain.EdgeCheckRecord) { r.Observation.StreamID = testHubID },
		"wrong revision":   func(r *domain.EdgeCheckRecord) { r.Observation.ConfigRevision = 2 },
		"wrong generation": func(r *domain.EdgeCheckRecord) { r.Observation.AssignmentGeneration = 2 },
		"unknown status":   func(r *domain.EdgeCheckRecord) { r.Observation.Status = domain.StatusUnknown },
	} {
		t.Run(name, func(t *testing.T) {
			record := checkRecord()
			mutate(&record)
			if _, err := s.CommitEdgeCheck(t.Context(), record); err == nil {
				t.Fatal("invalid authority/evidence recorded")
			}
		})
	}
	if _, err := s.db.ExecContext(t.Context(), "UPDATE edge_identity SET last_created_seq = ?", int64(math.MaxInt64-1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitEdgeCheck(t.Context(), checkRecord()); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("two-event overflow accepted: %v", err)
	}
	if _, err := s.db.ExecContext(t.Context(), "UPDATE edge_identity SET last_created_seq = 0"); err != nil {
		t.Fatal(err)
	}
	paused := protectedConfig(t, 2)
	paused.Assignments[0].Active = false
	if err := s.ActivateConfig(t.Context(), paused); err != nil {
		t.Fatal(err)
	}
	record := checkRecord()
	record.Observation.ConfigRevision = 2
	if _, err := s.CommitEdgeCheck(t.Context(), record); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("paused assignment recorded: %v", err)
	}
}
