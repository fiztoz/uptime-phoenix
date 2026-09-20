package edge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func ackFixture(t *testing.T) (*Store, string, domain.EdgeCommandAuthority, domain.ProbeAlertAcknowledgement) {
	t.Helper()
	s, dir := setupEdgeDeliveryStore(t)
	r := checkRecord()
	if _, err := s.CommitEdgeCheck(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.commandNow = func() time.Time { return now }
	a := domain.EdgeCommandAuthority{HubID: testHubID, ProbeID: testIdentity().ProbeID, StreamID: testIdentity().StreamID, ConnectionGeneration: 1}
	note := "Investigating connectivity"
	c := domain.ProbeAlertAcknowledgement{CommandID: "4106e52d-b157-4a16-b568-52bf3d518f03", ProbeID: a.ProbeID, SourceAlertID: r.Incident.SourceAlertID, AssignmentGeneration: 1, CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), ActorDisplayName: "Operator", Note: &note, PayloadHash: sha256.Sum256([]byte("immutable request"))}
	return s, dir, a, c
}

func assertAckStorage(t *testing.T, s *Store, status string, version, seq int64, receipts int) {
	t.Helper()
	e, err := s.ReadEdgeEvidence(t.Context(), 17, 1)
	if err != nil || e.Incident == nil || e.Incident.Status != status || e.Incident.TransitionVersion != version || e.State.Seq != 1 {
		t.Fatalf("wrong incident or observation evidence: %+v %v", e, err)
	}
	i, err := s.ReadIdentity(t.Context())
	if err != nil || i.LastCreatedSeq != seq {
		t.Fatalf("wrong telemetry counter: %+v %v", i, err)
	}
	count, err := s.db.NewSelect().Model((*edgeCommandRow)(nil)).Count(t.Context())
	if err != nil || count != receipts {
		t.Fatalf("wrong receipt count: %d %v", count, err)
	}
}

func TestEdgeAcknowledgementAtomicRollback(t *testing.T) {
	for _, fault := range []string{
		"BEFORE UPDATE ON edge_alerts",
		"BEFORE INSERT ON edge_telemetry_outbox",
		"BEFORE UPDATE OF last_created_seq ON edge_identity WHEN NEW.last_created_seq <> OLD.last_created_seq",
		"BEFORE INSERT ON edge_applied_commands",
	} {
		t.Run(fault, func(t *testing.T) {
			s, _, a, c := ackFixture(t)
			if _, err := s.db.ExecContext(t.Context(), "CREATE TRIGGER fail_ack "+fault+" BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
				t.Fatal(err)
			}
			out, err := s.ApplyAlertAcknowledgement(t.Context(), a, c)
			if !errors.Is(err, ErrStorage) || out.Status != "" {
				t.Fatalf("failed transaction reported success: %+v %v", out, err)
			}
			assertAckStorage(t, s, domain.AlertStatusFiring, 1, 2, 0)
			if _, err := s.db.ExecContext(t.Context(), "DROP TRIGGER fail_ack"); err != nil {
				t.Fatal(err)
			}
			out, err = s.ApplyAlertAcknowledgement(t.Context(), a, c)
			if err != nil || out.Status != "applied" {
				t.Fatalf("retry after rollback: %+v %v", out, err)
			}
			assertAckStorage(t, s, domain.AlertStatusAcked, 2, 3, 1)
		})
	}
}

func TestEdgeAcknowledgementReceiptRestartAndReconnect(t *testing.T) {
	s, dir, a, c := ackFixture(t)
	want, err := s.ApplyAlertAcknowledgement(t.Context(), a, c)
	if err != nil || want.Status != "applied" || want.AppliedAt == nil || want.AppliedAt.Location() != time.UTC {
		t.Fatalf("ACK failed: %+v %v", want, err)
	}
	var payload []byte
	if err := s.db.NewRaw("SELECT payload FROM edge_telemetry_outbox WHERE seq = 3").Scan(t.Context(), &payload); err != nil {
		t.Fatal(err)
	}
	var event struct {
		Kind string                   `json:"kind"`
		Data probe.IncidentTransition `json:"data"`
	}
	if err := json.Unmarshal(payload, &event); err != nil || event.Kind != "alert.transition" || event.Data.Status != "acked" || event.Data.Acknowledgement == nil || event.Data.Acknowledgement.CommandID != c.CommandID || event.Data.Acknowledgement.ActorDisplayName != c.ActorDisplayName || *event.Data.Acknowledgement.Note != *c.Note {
		t.Fatalf("lost ACK replay correlation: %+v %v", event, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), dir, testIdentity(), WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	// The original request has expired; its committed result still survives.
	s.commandNow = func() time.Time { return c.ExpiresAt.Add(time.Hour) }
	if err := s.AcceptConnectionGeneration(t.Context(), a.HubID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyAlertAcknowledgement(t.Context(), a, c); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("old session recovered receipt: %v", err)
	}
	a.ConnectionGeneration = 2
	got, err := s.ApplyAlertAcknowledgement(t.Context(), a, c)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("lost immutable receipt: %+v %v", got, err)
	}
	assertAckStorage(t, s, domain.AlertStatusAcked, 2, 3, 1)
}

func TestEdgeAcknowledgementRejectsConflictingIdentity(t *testing.T) {
	s, _, a, c := ackFixture(t)
	if _, err := s.ApplyAlertAcknowledgement(t.Context(), a, c); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*domain.EdgeCommandAuthority, *domain.ProbeAlertAcknowledgement){
		"raw bytes": func(_ *domain.EdgeCommandAuthority, c *domain.ProbeAlertAcknowledgement) { c.PayloadHash[0]++ },
		"actor with same digest": func(_ *domain.EdgeCommandAuthority, c *domain.ProbeAlertAcknowledgement) {
			c.ActorDisplayName = "Another operator"
		},
		"note":       func(_ *domain.EdgeCommandAuthority, c *domain.ProbeAlertAcknowledgement) { c.Note = nil },
		"incident":   func(_ *domain.EdgeCommandAuthority, c *domain.ProbeAlertAcknowledgement) { c.SourceAlertID = testHubID },
		"generation": func(_ *domain.EdgeCommandAuthority, c *domain.ProbeAlertAcknowledgement) { c.AssignmentGeneration++ },
		"expiry": func(_ *domain.EdgeCommandAuthority, c *domain.ProbeAlertAcknowledgement) {
			c.ExpiresAt = c.ExpiresAt.Add(time.Nanosecond)
		},
		"hub":    func(a *domain.EdgeCommandAuthority, _ *domain.ProbeAlertAcknowledgement) { a.HubID = a.StreamID },
		"stream": func(a *domain.EdgeCommandAuthority, _ *domain.ProbeAlertAcknowledgement) { a.StreamID = a.HubID },
		"probe":  func(a *domain.EdgeCommandAuthority, _ *domain.ProbeAlertAcknowledgement) { a.ProbeID = a.HubID },
	} {
		t.Run(name, func(t *testing.T) {
			aa, cc := a, c
			change(&aa, &cc)
			if _, err := s.ApplyAlertAcknowledgement(t.Context(), aa, cc); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("conflict accepted: %v", err)
			}
		})
	}
	assertAckStorage(t, s, domain.AlertStatusAcked, 2, 3, 1)
}

func TestEdgeAcknowledgementExpiryTargetAndDuplicateOperators(t *testing.T) {
	for _, scenario := range []string{"expired", "future", "missing", "wrong generation", "already acked", "retired assignment"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, a, c := ackFixture(t)
			want := "rejected"
			seq, version, status := int64(2), int64(1), domain.AlertStatusFiring
			count := 1
			switch scenario {
			case "expired":
				c.ExpiresAt = c.CreatedAt.Add(time.Second)
				want = "expired"
			case "future":
				c.CreatedAt = c.CreatedAt.Add(2 * time.Minute)
			case "missing":
				c.SourceAlertID = testHubID
			case "wrong generation":
				c.AssignmentGeneration++
			case "already acked":
				if _, err := s.ApplyAlertAcknowledgement(t.Context(), a, c); err != nil {
					t.Fatal(err)
				}
				c.CommandID, c.ActorDisplayName = "76398131-cd97-4c46-a23c-baf99016d725", "Second operator"
				seq, version, status, count, want = 3, 2, domain.AlertStatusAcked, 2, "already_applied"
			case "retired assignment":
				config := protectedConfig(t, 2)
				config.Assignments = nil
				if err := s.ActivateConfig(t.Context(), config); err != nil {
					t.Fatal(err)
				}
				seq, version, status, want = 3, 2, domain.AlertStatusAcked, "applied"
			}
			out, err := s.ApplyAlertAcknowledgement(t.Context(), a, c)
			if err != nil || out.Status != want {
				t.Fatalf("wrong outcome: %+v %v", out, err)
			}
			assertAckStorage(t, s, status, version, seq, count)
			again, err := s.ApplyAlertAcknowledgement(t.Context(), a, c)
			if err != nil || !reflect.DeepEqual(out, again) {
				t.Fatal("terminal result changed on duplicate", err)
			}
			if scenario == "already acked" {
				e, err := s.ReadEdgeEvidence(t.Context(), 17, 1)
				if err != nil || e.Incident.AckActorDisplayName != "Operator" {
					t.Fatal("second operator overwrote original ACK", err)
				}
			}
		})
	}
}

func TestEdgeAcknowledgementConcurrentDuplicate(t *testing.T) {
	s, _, a, c := ackFixture(t)
	var wg sync.WaitGroup
	results := make(chan domain.ProbeCommandOutcome, 8)
	errors := make(chan error, 8)
	for range 8 {
		wg.Go(func() { out, err := s.ApplyAlertAcknowledgement(t.Context(), a, c); results <- out; errors <- err })
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first *domain.ProbeCommandOutcome
	for out := range results {
		if first == nil {
			first = &out
		} else if !reflect.DeepEqual(*first, out) {
			t.Fatal("duplicate receipts disagree")
		}
	}
	assertAckStorage(t, s, domain.AlertStatusAcked, 2, 3, 1)
}

func TestEdgeAcknowledgementFencesChecksAndPreservesRecovery(t *testing.T) {
	s, _, a, c := ackFixture(t)
	// Capture a pre-ACK recovery proposal, including its independent incident CAS.
	up := checkRecord()
	up.ExpectedStateSeq, up.ExpectedIncidentVersion = 1, 1
	up.Observation.Status, up.Observation.RawStatus, up.Observation.DownCount = domain.StatusUp, domain.StatusUp, 0
	up.Incident.Status, up.Incident.TransitionVersion, up.Incident.ResolvedAt = domain.AlertStatusResolved, 2, &up.Observation.ObservedAt
	up.DeliveryIntents = nil
	if _, err := s.ApplyAlertAcknowledgement(t.Context(), a, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitEdgeCheck(t.Context(), up); !errors.Is(err, ports.ErrStaleLocalState) {
		t.Fatal("pre-ACK check bypassed incident CAS", err)
	}
	e, err := s.ReadEdgeEvidence(t.Context(), 17, 1)
	if err != nil {
		t.Fatal(err)
	}
	up.ExpectedIncidentVersion = e.Incident.TransitionVersion
	up.Incident = e.Incident
	up.Incident.Status, up.Incident.TransitionVersion, up.Incident.ResolvedAt = domain.AlertStatusResolved, 3, &up.Observation.ObservedAt
	for _, mutate := range []func(*domain.RegionalIncident){
		func(i *domain.RegionalIncident) { i.AckCommandID = testHubID },
		func(i *domain.RegionalIncident) { i.AckedAt = nil },
		func(i *domain.RegionalIncident) { i.AckNote = nil },
	} {
		bad := up
		copy := *up.Incident
		bad.Incident = &copy
		mutate(bad.Incident)
		if _, err := s.CommitEdgeCheck(t.Context(), bad); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("check changed ACK", err)
		}
	}
	resolved, err := s.CommitEdgeCheck(t.Context(), up)
	if err != nil || resolved.Seq != 4 {
		t.Fatal("ACK prevented recovery", err)
	}
	e, err = s.ReadEdgeEvidence(t.Context(), 17, 1)
	if err != nil || e.Incident.AckCommandID != c.CommandID || e.Incident.Status != domain.AlertStatusResolved || e.Incident.TransitionVersion != 3 {
		t.Fatal("recovery lost ACK", err)
	}
	// A later outage is a new identity. The late original-target ACK must leave it firing.
	down := checkRecord()
	down.ExpectedStateSeq, down.ExpectedIncidentVersion = 4, 3
	down.Incident.SourceAlertID = "c1aef447-0510-4c72-ab68-fd9176f6fd53"
	down.DeliveryIntents = nil
	if _, err := s.CommitEdgeCheck(t.Context(), down); err != nil {
		t.Fatal(err)
	}
	c.CommandID = "76398131-cd97-4c46-a23c-baf99016d725"
	out, err := s.ApplyAlertAcknowledgement(t.Context(), a, c)
	if err != nil || out.Status != "already_resolved" {
		t.Fatalf("late ACK retargeted: %+v %v", out, err)
	}
	e, err = s.ReadEdgeEvidence(t.Context(), 17, 1)
	if err != nil || e.Incident.SourceAlertID != down.Incident.SourceAlertID || e.Incident.Status != domain.AlertStatusFiring || e.Incident.AckedAt != nil {
		t.Fatal("new outage was acknowledged", err)
	}
}

func TestEdgeAcknowledgementSourceServiceLifecycle(t *testing.T) {
	s, _, authority, command := ackFixture(t)
	active, err := s.ReadActiveConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	assignment := domain.EdgeResolvedAssignment{Monitor: &domain.Monitor{ID: 17, Active: true, Type: "http", ResendInterval: 1}, Generation: 1, NotificationLinks: []domain.MonitorNotification{{MonitorID: 17, NotificationID: 10}}}
	config := &domain.EdgeResolvedConfig{Metadata: active.Snapshot.ProbeConfigMetadata, Assignments: []domain.EdgeResolvedAssignment{assignment}, Channels: map[int64]domain.EdgeResolvedChannel{10: {Notification: &domain.Notification{ID: 10, Type: "webhook", Active: true}, Version: 1}}}
	svc := services.NewEdgeRecordingService(s, s, nil)
	if _, err := s.ApplyAlertAcknowledgement(t.Context(), authority, command); err != nil {
		t.Fatal(err)
	}
	at := s.commandNow()
	for n, status := range []domain.Status{domain.StatusDown, domain.StatusUp, domain.StatusDown} {
		if _, err := svc.Record(t.Context(), config, assignment, ports.CheckResult{Status: status}, at.Add(time.Duration(n+1)*2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		e, err := s.ReadEdgeEvidence(t.Context(), 17, 1)
		if err != nil {
			t.Fatal(err)
		}
		count, err := s.db.NewSelect().Model((*edgeDeliveryRow)(nil)).Count(t.Context())
		if err != nil || count != n+1 {
			t.Fatal("ACK suppressed recovery or allowed resend", count, err)
		}
		switch n {
		case 0:
			if e.Incident.Status != domain.AlertStatusAcked || e.Incident.TransitionVersion != 2 {
				t.Fatal("DOWN reset ACK")
			}
		case 1:
			if e.Incident.Status != domain.AlertStatusResolved || e.Incident.AckCommandID != command.CommandID || e.Incident.TransitionVersion != 3 {
				t.Fatal("recovery lost ACK")
			}
		case 2:
			if e.Incident.Status != domain.AlertStatusFiring || e.Incident.SourceAlertID == command.SourceAlertID || e.Incident.AckedAt != nil {
				t.Fatal("ACK silenced future outage")
			}
		}
	}
}

func TestEdgeAcknowledgementBoundsAndRetainsReceipts(t *testing.T) {
	s, _, a, c := ackFixture(t)
	want, err := s.ApplyAlertAcknowledgement(t.Context(), a, c)
	if err != nil {
		t.Fatal(err)
	}
	// Fill the independent receipt ledger while preserving all real source state.
	if _, err := s.db.ExecContext(t.Context(), "WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x < ?) INSERT INTO edge_applied_commands (command_id,kind,request_hash,status,applied_at,code,message,retain_until) SELECT printf('fixture-%d',x),'alert.ack',zeroblob(32),'rejected',NULL,'target_not_found','Command target was not found',? FROM n", maxAppliedCommands-1, c.ExpiresAt.Add(commandRetention).UnixMicro()); err != nil {
		t.Fatal(err)
	}
	next := c
	next.CommandID = "76398131-cd97-4c46-a23c-baf99016d725"
	if _, err := s.ApplyAlertAcknowledgement(t.Context(), a, next); !errors.Is(err, ErrQueueFull) {
		t.Fatal("unbounded receipt storage", err)
	}
	got, err := s.ApplyAlertAcknowledgement(t.Context(), a, c)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("pressure discarded known receipt", err)
	}
	now := c.ExpiresAt.Add(commandRetention + time.Hour)
	s.commandNow = func() time.Time { return now }
	next.CreatedAt, next.ExpiresAt = now, now.Add(time.Hour)
	got, err = s.ApplyAlertAcknowledgement(t.Context(), a, next)
	if err != nil || got.Status != "already_applied" {
		t.Fatalf("expired ledger failed to reclaim safely: %+v %v", got, err)
	}
	count, err := s.db.NewSelect().Model((*edgeCommandRow)(nil)).Count(t.Context())
	if err != nil || count != 1 {
		t.Fatal("old receipts were not bounded", count, err)
	}
	// An evicted request cannot apply again after its immutable expiry.
	got, err = s.ApplyAlertAcknowledgement(t.Context(), a, c)
	if err != nil || got.Status != "expired" {
		t.Fatal("expired request reapplied after cleanup", err)
	}
	assertAckStorage(t, s, domain.AlertStatusAcked, 2, 3, 2)
}

func TestEdgeAcknowledgementSuppressesAuthorizedDownAndKeepsLeaseReceipt(t *testing.T) {
	s, _, a, c := ackFixture(t)
	config, err := s.ReadActiveConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.ClaimDeliveries(t.Context(), a.ProbeID, time.Now().UTC(), time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatal("claim failed", err)
	}
	claim := domain.DeliveryClaim{DeliveryID: items[0].DeliveryID, ProbeID: a.ProbeID, Attempt: items[0].Attempt, LeaseToken: items[0].LeaseToken}
	before, err := s.AuthorizeEdgeDelivery(t.Context(), claim, config.Snapshot.ProbeConfigMetadata, 10*time.Second)
	if err != nil || before == nil {
		t.Fatal("firing delivery was not authorized", err)
	}
	if _, err := s.ApplyAlertAcknowledgement(t.Context(), a, c); err != nil {
		t.Fatal(err)
	}
	after, err := s.AuthorizeEdgeDelivery(t.Context(), claim, config.Snapshot.ProbeConfigMetadata, 10*time.Second)
	if err != nil || after != nil {
		t.Fatal("ACK did not suppress DOWN at authorization", err)
	}
	if err := s.FinishDelivery(t.Context(), claim, domain.DeliveryResult{Status: domain.DeliveryStatusSuperseded, At: time.Now().UTC()}); err != nil {
		t.Fatal("ACK lost delivery lease or reservation", err)
	}
	item, err := s.GetDeliveryIntent(t.Context(), a.ProbeID, claim.DeliveryID)
	if err != nil || item.Status != domain.DeliveryStatusSuperseded {
		t.Fatal("suppression outcome was not persisted", err)
	}
	assertAckStorage(t, s, domain.AlertStatusAcked, 2, 4, 1)
}

func TestEdgeAcknowledgementMigrationRefusesReceiptLoss(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty ledger", true: "retained receipt"}[applied], func(t *testing.T) {
			s, _, a, c := ackFixture(t)
			if applied {
				if _, err := s.ApplyAlertAcknowledgement(t.Context(), a, c); err != nil {
					t.Fatal(err)
				}
			}
			down, err := migrations.ReadFile("migrations/006_applied_commands.tx.down.sql")
			if err != nil {
				t.Fatal(err)
			}
			err = s.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error { _, err := tx.ExecContext(ctx, string(down)); return err })
			if applied {
				if err == nil {
					t.Fatal("downgrade discarded idempotency receipt")
				}
				assertAckStorage(t, s, domain.AlertStatusAcked, 2, 3, 1)
			} else if err != nil {
				t.Fatal("empty ledger could not downgrade", err)
			}
		})
	}
}

func TestEdgeRegionalDeliveryAuthority(t *testing.T) {
	for _, scenario := range []string{"stale claim", "expired lease", "short lease", "replaced config", "acked recovery after next outage"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, a, c := ackFixture(t)
			config, err := s.ReadActiveConfig(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			items, err := s.ClaimDeliveries(t.Context(), a.ProbeID, time.Now().UTC(), time.Minute, 1)
			if err != nil || len(items) != 1 {
				t.Fatal("claim failed", err)
			}
			claim := domain.DeliveryClaim{DeliveryID: items[0].DeliveryID, ProbeID: a.ProbeID, Attempt: items[0].Attempt, LeaseToken: items[0].LeaseToken}
			switch scenario {
			case "stale claim":
				claim.Attempt++
			case "expired lease", "short lease":
				until := time.Now().Add(5 * time.Second)
				if scenario == "expired lease" {
					until = time.Now().Add(-time.Second)
				}
				if _, err := s.db.ExecContext(t.Context(), "UPDATE edge_delivery_outbox SET leased_at = ?, lease_until = ?", until.Add(-time.Minute).UnixMicro(), until.UnixMicro()); err != nil {
					t.Fatal(err)
				}
			case "replaced config":
				if err := s.ActivateConfig(t.Context(), protectedConfig(t, 2)); err != nil {
					t.Fatal(err)
				}
			case "acked recovery after next outage":
				if _, err := s.ApplyAlertAcknowledgement(t.Context(), a, c); err != nil {
					t.Fatal(err)
				}
				e, err := s.ReadEdgeEvidence(t.Context(), 17, 1)
				if err != nil {
					t.Fatal(err)
				}
				up := checkRecord()
				up.ExpectedStateSeq, up.ExpectedIncidentVersion = 1, 2
				up.Observation.Status, up.Observation.RawStatus, up.Observation.DownCount = domain.StatusUp, domain.StatusUp, 0
				up.Incident = e.Incident
				up.Incident.Status, up.Incident.ResolvedAt, up.Incident.TransitionVersion = domain.AlertStatusResolved, &up.Observation.ObservedAt, 3
				up.DeliveryIntents[0].DeliveryID = "ce9590f7-ac7b-4355-acf0-8456a10bfd29"
				up.DeliveryIntents[0].SourceTransitionVersion = 3
				if _, err := s.CommitEdgeCheck(t.Context(), up); err != nil {
					t.Fatal(err)
				}
				down := checkRecord()
				down.ExpectedStateSeq, down.ExpectedIncidentVersion = 4, 3
				down.Incident.SourceAlertID = "c1aef447-0510-4c72-ab68-fd9176f6fd53"
				down.DeliveryIntents = nil
				if _, err := s.CommitEdgeCheck(t.Context(), down); err != nil {
					t.Fatal(err)
				}
				items, err = s.ClaimDeliveries(t.Context(), a.ProbeID, time.Now().UTC(), time.Minute, 1)
				if err != nil || len(items) != 1 || items[0].CheckStatus != domain.StatusUp {
					t.Fatal("recovery not queued", err)
				}
				claim = domain.DeliveryClaim{DeliveryID: items[0].DeliveryID, ProbeID: a.ProbeID, Attempt: items[0].Attempt, LeaseToken: items[0].LeaseToken}
			}
			got, err := s.AuthorizeEdgeDelivery(t.Context(), claim, config.Snapshot.ProbeConfigMetadata, 10*time.Second)
			switch scenario {
			case "replaced config":
				if err != nil || got != nil {
					t.Fatal("obsolete config authorized", err)
				}
			case "acked recovery after next outage":
				if err != nil || got == nil || got.CheckStatus != domain.StatusUp {
					t.Fatal("ACK or next outage suppressed original recovery", err)
				}
			default:
				if !errors.Is(err, ports.ErrConflict) || got != nil {
					t.Fatal("stale attempt authorized", err)
				}
			}
		})
	}
}
