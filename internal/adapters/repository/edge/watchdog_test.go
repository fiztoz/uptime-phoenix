package edge

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func watchdogFixture(t *testing.T) (*Store, string, domain.ProbeWatchdogAuthority) {
	t.Helper()
	s, dir := testStore(t)
	s.telemetry = probe.EdgeTelemetryEncoder{}
	enroll(t, s)
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 1)); err != nil {
		t.Fatal(err)
	}
	i := testIdentity()
	return s, dir, domain.ProbeWatchdogAuthority{HubID: testHubID, ProbeID: i.ProbeID, StreamID: i.StreamID, HealthGeneration: 1}
}
func watchdogOpening() domain.ProbeWatchdogRecord {
	at := time.Now().UTC().Truncate(time.Microsecond)
	inc := &domain.RegionalIncident{SourceAlertID: "197ea258-8d9b-4fdf-a1e0-a9b72edfbf82", ProbeID: testIdentity().ProbeID, Scope: domain.IncidentScopeProbeConnection, SubjectKind: domain.IncidentSubjectWatchdog, Status: domain.AlertStatusFiring, TransitionVersion: 1, StartedAt: at, ConfigRevision: 1, Reason: "Hub durable ingestion unavailable"}
	intent := domain.DeliveryIntent{DeliveryID: "f3d93199-1731-4c64-9345-c6eebf9babd3", SourceAlertID: inc.SourceAlertID, SourceTransitionVersion: 1, ProbeID: inc.ProbeID, NotificationID: 10, NotificationVersion: 1, EventKind: domain.DeliveryEventProbeConnection, AvailableAt: at}
	return domain.ProbeWatchdogRecord{ConfigRevision: 1, Checkpoint: domain.ProbeWatchdogCheckpoint{Armed: true, LossElapsed: 90 * time.Second, PendingLoss: true}, Status: domain.ProbeWatchdogLost, At: at, Incident: inc, DeliveryIntents: []domain.DeliveryIntent{intent}}
}
func requireWatchdogEmpty(t *testing.T, s *Store) {
	t.Helper()
	i, err := s.ReadIdentity(t.Context())
	if err != nil || i.LastCreatedSeq != 0 {
		t.Fatalf("partial sequence: %+v %v", i, err)
	}
	for _, table := range []string{"edge_alerts", "edge_telemetry_outbox", "edge_delivery_outbox", "edge_watchdog_state", "edge_regional_state"} {
		count, err := s.db.NewSelect().Table(table).Count(t.Context())
		if err != nil || count != 0 {
			t.Fatalf("%s leaked %d rows: %v", table, count, err)
		}
	}
}

func TestEdgeWatchdogAtomicEffects(t *testing.T) {
	s, _, authority := watchdogFixture(t)
	for _, table := range []string{"edge_alerts", "edge_telemetry_outbox", "edge_delivery_outbox", "edge_watchdog_state"} {
		t.Run(table, func(t *testing.T) {
			if _, err := s.db.ExecContext(t.Context(), "CREATE TRIGGER fail_watchdog BEFORE INSERT ON "+table+" BEGIN SELECT RAISE(ABORT,'watchdog fault'); END"); err != nil {
				t.Fatal(err)
			}
			if got, err := s.CommitWatchdog(t.Context(), authority, watchdogOpening()); err == nil || got.Version != 0 {
				t.Fatalf("partial commit: %+v %v", got, err)
			}
			if _, err := s.db.ExecContext(t.Context(), "DROP TRIGGER fail_watchdog"); err != nil {
				t.Fatal(err)
			}
			requireWatchdogEmpty(t, s)
		})
	}
	state, err := s.CommitWatchdog(t.Context(), authority, watchdogOpening())
	if err != nil || state.Version != 1 || state.IncidentSeq != 1 {
		t.Fatalf("opening: %+v %v", state, err)
	}
	var nulls int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_alerts WHERE scope='probe_connection' AND monitor_id IS NULL AND generation IS NULL").Scan(t.Context(), &nulls); err != nil || nulls != 1 {
		t.Fatalf("incident fabricated monitor: %d %v", nulls, err)
	}
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_delivery_outbox WHERE event_kind='probe_connection' AND monitor_id IS NULL AND generation IS NULL").Scan(t.Context(), &nulls); err != nil || nulls != 1 {
		t.Fatalf("delivery fabricated monitor: %d %v", nulls, err)
	}
	var payload []byte
	if err := s.db.NewRaw("SELECT payload FROM edge_telemetry_outbox WHERE seq=1").Scan(t.Context(), &payload); err != nil {
		t.Fatal(err)
	}
	var event struct {
		Kind string                     `json:"kind"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if event.Kind != "watchdog.transition" || string(event.Data["monitor_id"]) != "null" || string(event.Data["assignment_generation"]) != "null" {
		t.Fatalf("wrong probe wire scope: %s", payload)
	}
	if n, err := s.db.NewSelect().Table("edge_regional_state").Count(t.Context()); err != nil || n != 0 {
		t.Fatalf("watchdog changed monitor health: %d %v", n, err)
	}
}

func TestEdgeWatchdogRestartAckRecoveryAndOutcome(t *testing.T) {
	s, dir, authority := watchdogFixture(t)
	ctx := t.Context()
	opening := watchdogOpening()
	state, err := s.CommitWatchdog(ctx, authority, opening)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := opening
	checkpoint.ExpectedVersion = state.Version
	checkpoint.Incident = nil
	checkpoint.DeliveryIntents = nil
	state, err = s.CommitWatchdog(ctx, authority, checkpoint)
	if err != nil || state.Version != 2 || state.IncidentSeq != 1 {
		t.Fatalf("checkpoint added event: %+v %v", state, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir, testIdentity(), WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func(opened *Store) { _ = opened.Close() }(s)
	restored, err := s.ReadWatchdog(ctx, authority)
	if err != nil || !reflect.DeepEqual(restored, state) {
		t.Fatalf("restart lost source identity: %+v %v", restored, err)
	}
	if _, err := s.CommitWatchdog(ctx, authority, opening); !errors.Is(err, ports.ErrStaleLocalState) {
		t.Fatalf("duplicate opening replayed: %v", err)
	}
	ack := *restored.Incident
	ack.Status = domain.AlertStatusAcked
	ack.TransitionVersion++
	ack.AckedAt = &opening.At
	ack.AckCommandID = "e81da04d-c9f1-4165-a3cb-94459c26ece8"
	ack.AckActorDisplayName = "Operator"
	note := "Investigating"
	ack.AckNote = &note
	checkpoint.ExpectedVersion = restored.Version
	checkpoint.Incident = &ack
	state, err = s.CommitWatchdog(ctx, authority, checkpoint)
	if err != nil || state.Incident.Status != domain.AlertStatusAcked || state.IncidentSeq != 2 {
		t.Fatalf("ack state: %+v %v", state, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir, testIdentity(), WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func(opened *Store) { _ = opened.Close() }(s)
	restored, err = s.ReadWatchdog(ctx, authority)
	if err != nil || restored.Incident.AckCommandID != ack.AckCommandID || restored.Incident.AckNote == nil || *restored.Incident.AckNote != note {
		t.Fatalf("restart forgot acknowledgement: %+v %v", restored, err)
	}
	recovered := *restored.Incident
	recovered.Status = domain.AlertStatusResolved
	recovered.TransitionVersion++
	// Preserve original source wall time even across a correction. Timer timing
	// and source lifecycle versions do not depend on wall-clock order.
	at := opening.At.Add(-time.Minute)
	recovered.ResolvedAt = &at
	checkpoint.ExpectedVersion = restored.Version
	checkpoint.At = at
	checkpoint.Status = domain.ProbeWatchdogHealthy
	checkpoint.Checkpoint = domain.ProbeWatchdogCheckpoint{Armed: true}
	checkpoint.Incident = &recovered
	intent := opening.DeliveryIntents[0]
	intent.DeliveryID = "b349fa1a-fb1a-4675-96bd-46b0d7fce074"
	intent.SourceTransitionVersion = 3
	intent.AvailableAt = at
	checkpoint.DeliveryIntents = []domain.DeliveryIntent{intent}
	state, err = s.CommitWatchdog(ctx, authority, checkpoint)
	if err != nil || state.IncidentSeq != 3 || state.Incident.SourceAlertID != opening.Incident.SourceAlertID {
		t.Fatalf("recovery: %+v %v", state, err)
	}
	items, err := s.ClaimDeliveries(ctx, authority.ProbeID, opening.At.Add(time.Second), time.Minute, 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("probe work not claimable: %+v %v", items, err)
	}
	for _, item := range items {
		if item.MonitorID != 0 || item.AssignmentGeneration != 0 || item.EventKind != domain.DeliveryEventProbeConnection {
			t.Fatalf("wrong entity: %+v", item)
		}
		outcome := domain.DeliveryStatusSent
		if item.CheckStatus == domain.StatusDown {
			outcome = domain.DeliveryStatusSuperseded
		}
		if err := s.FinishDelivery(ctx, domain.DeliveryClaim{DeliveryID: item.DeliveryID, ProbeID: item.ProbeID, Attempt: item.Attempt, LeaseToken: item.LeaseToken}, domain.DeliveryResult{Status: outcome, At: opening.At.Add(2 * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	identity, err := s.ReadIdentity(ctx)
	if err != nil || identity.LastCreatedSeq != 5 {
		t.Fatalf("source event sequence: %+v %v", identity, err)
	}
	if n, err := s.db.NewSelect().Table("edge_alerts").Count(ctx); err != nil || n != 1 {
		t.Fatalf("restart duplicated incident: %d %v", n, err)
	}
}

func TestEdgeWatchdogAuthorityAndVersionFences(t *testing.T) {
	s, _, authority := watchdogFixture(t)
	for name, mutate := range map[string]func(*domain.ProbeWatchdogAuthority, *domain.ProbeWatchdogRecord){
		"foreign installation":     func(a *domain.ProbeWatchdogAuthority, _ *domain.ProbeWatchdogRecord) { a.HubID = a.StreamID },
		"foreign probe":            func(a *domain.ProbeWatchdogAuthority, _ *domain.ProbeWatchdogRecord) { a.ProbeID = a.StreamID },
		"foreign stream":           func(a *domain.ProbeWatchdogAuthority, _ *domain.ProbeWatchdogRecord) { a.StreamID = a.ProbeID },
		"future health generation": func(a *domain.ProbeWatchdogAuthority, _ *domain.ProbeWatchdogRecord) { a.HealthGeneration = 2 },
		"hub runtime authority":    func(a *domain.ProbeWatchdogAuthority, _ *domain.ProbeWatchdogRecord) { a.RuntimeOwner.Epoch = 1 },
		"obsolete configuration":   func(_ *domain.ProbeWatchdogAuthority, r *domain.ProbeWatchdogRecord) { r.ConfigRevision = 2 },
		"missing incident": func(_ *domain.ProbeWatchdogAuthority, r *domain.ProbeWatchdogRecord) {
			r.Incident = nil
			r.DeliveryIntents = nil
		},
		"fake monitor": func(_ *domain.ProbeWatchdogAuthority, r *domain.ProbeWatchdogRecord) { r.Incident.MonitorID = 17 },
		"wrong channel version": func(_ *domain.ProbeWatchdogAuthority, r *domain.ProbeWatchdogRecord) {
			r.DeliveryIntents[0].NotificationVersion = 2
		},
		"version overflow": func(_ *domain.ProbeWatchdogAuthority, r *domain.ProbeWatchdogRecord) {
			r.ExpectedVersion = math.MaxInt64
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, r := authority, watchdogOpening()
			mutate(&a, &r)
			if _, err := s.CommitWatchdog(t.Context(), a, r); err == nil {
				t.Fatal("invalid source authority accepted")
			}
			requireWatchdogEmpty(t, s)
		})
	}
	if err := s.AcceptConnectionGeneration(t.Context(), authority.HubID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitWatchdog(t.Context(), authority, watchdogOpening()); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("old health committed: %v", err)
	}
	authority.HealthGeneration = 0 // Local timer may record loss while disconnected.
	if _, err := s.CommitWatchdog(t.Context(), authority, watchdogOpening()); err != nil {
		t.Fatal(err)
	}
}

func runWatchdogMigration(t *testing.T, s *Store, direction string, failAfter bool) error {
	t.Helper()
	sql, err := migrations.ReadFile("migrations/005_watchdog_source.tx." + direction + ".sql")
	if err != nil {
		t.Fatal(err)
	}
	if failAfter {
		sql = append(sql, []byte("\nSELECT * FROM injected_missing_migration_table;\n")...)
	}
	return s.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error { _, err := tx.ExecContext(ctx, string(sql)); return err })
}

func TestEdgeWatchdogMigrationPreservesExistingWork(t *testing.T) {
	s, _, _ := watchdogFixture(t)
	ctx := t.Context()
	if _, err := s.CommitEdgeCheck(ctx, checkRecord()); err != nil {
		t.Fatal(err)
	}
	before, err := s.ClaimDeliveries(ctx, testIdentity().ProbeID, time.Now().UTC().Add(time.Second), time.Minute, 1)
	if err != nil || len(before) != 1 {
		t.Fatalf("claim: %+v %v", before, err)
	}
	var original []byte
	if err := s.db.NewRaw("SELECT payload FROM edge_telemetry_outbox WHERE seq=2").Scan(ctx, &original); err != nil {
		t.Fatal(err)
	}
	if err := runWatchdogMigration(t, s, "down", false); err != nil {
		t.Fatal(err)
	}
	if err := runWatchdogMigration(t, s, "up", true); err == nil {
		t.Fatal("migration fault did not abort")
	}
	var count int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM pragma_table_info('edge_alerts') WHERE name='scope'").Scan(ctx, &count); err != nil || count != 0 {
		t.Fatalf("partial replacement escaped rollback: %d %v", count, err)
	}
	if err := runWatchdogMigration(t, s, "up", false); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetDeliveryIntent(ctx, testIdentity().ProbeID, before[0].DeliveryID)
	if err != nil || !reflect.DeepEqual(*after, before[0]) {
		t.Fatalf("migration changed lease/identity: %+v %v", after, err)
	}
	var payload []byte
	if err := s.db.NewRaw("SELECT payload FROM edge_telemetry_outbox WHERE seq=2").Scan(ctx, &payload); err != nil || string(payload) != string(original) {
		t.Fatalf("migration rewrote telemetry: %v", err)
	}
	var violations []struct {
		Table  string
		Rowid  int64
		Parent string
		Fkid   int
	}
	if err := s.db.NewRaw("PRAGMA foreign_key_check").Scan(ctx, &violations); err != nil || len(violations) != 0 {
		t.Fatalf("foreign keys: %+v %v", violations, err)
	}
}

func TestEdgeWatchdogDowngradeRetainsCheckpoint(t *testing.T) {
	s, _, authority := watchdogFixture(t)
	state, err := s.CommitWatchdog(t.Context(), authority, watchdogOpening())
	if err != nil {
		t.Fatal(err)
	}
	if err := runWatchdogMigration(t, s, "down", false); err == nil {
		t.Fatal("downgrade discarded source watchdog")
	}
	after, err := s.ReadWatchdog(t.Context(), authority)
	if err != nil || !reflect.DeepEqual(after, state) {
		t.Fatalf("downgrade changed state: %+v %v", after, err)
	}
}

func TestEdgeWatchdogCompetingCommitsAndDuplicateDelivery(t *testing.T) {
	s, _, authority := watchdogFixture(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	for n := 0; n < 2; n++ {
		go func() { <-start; _, err := s.CommitWatchdog(t.Context(), authority, watchdogOpening()); results <- err }()
	}
	close(start)
	successes, stale := 0, 0
	for n := 0; n < 2; n++ {
		err := <-results
		if err == nil {
			successes++
		} else if errors.Is(err, ports.ErrStaleLocalState) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("competing source writers: %d successes %d stale", successes, stale)
	}
	before, err := s.ReadWatchdog(t.Context(), authority)
	if err != nil {
		t.Fatal(err)
	}
	r := watchdogOpening()
	r.ExpectedVersion = before.Version
	r.Incident = nil
	if _, err := s.CommitWatchdog(t.Context(), authority, r); err == nil {
		t.Fatal("duplicate delivery ID accepted")
	}
	after, err := s.ReadWatchdog(t.Context(), authority)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed resend changed checkpoint: %+v %v", after, err)
	}
	i, err := s.ReadIdentity(t.Context())
	if err != nil || i.LastCreatedSeq != 1 {
		t.Fatalf("failed resend advanced sequence: %+v %v", i, err)
	}
	r.DeliveryIntents[0].DeliveryID = "ab61faf4-435a-4cf4-bf2e-6963573a94d9"
	after, err = s.CommitWatchdog(t.Context(), authority, r)
	if err != nil || after.IncidentSeq != 1 || after.Incident.TransitionVersion != 1 {
		t.Fatalf("resend changed lifecycle: %+v %v", after, err)
	}
	var seqs []int64
	if err := s.db.NewRaw("SELECT source_seq FROM edge_delivery_outbox ORDER BY delivery_id").Scan(t.Context(), &seqs); err != nil || len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 1 {
		t.Fatalf("resend lost source transition: %v %v", seqs, err)
	}
}

func TestEdgeWatchdogDisableReenableAndNewOutage(t *testing.T) {
	s, _, a := watchdogFixture(t)
	r := watchdogOpening()
	st, err := s.CommitWatchdog(t.Context(), a, r)
	if err != nil {
		t.Fatal(err)
	}
	resolved := *st.Incident
	resolved.Status = domain.AlertStatusResolved
	resolved.TransitionVersion++
	resolved.ResolvedAt = &r.At
	r.ExpectedVersion = st.Version
	r.Incident = &resolved
	r.DeliveryIntents = nil
	r.Status = domain.ProbeWatchdogUnarmed
	r.Checkpoint = domain.ProbeWatchdogCheckpoint{}
	st, err = s.CommitWatchdog(t.Context(), a, r)
	if err != nil {
		t.Fatal(err)
	}
	if st.Incident.Status != domain.AlertStatusResolved || st.Checkpoint.Armed {
		t.Fatal("disable retained open outage")
	}
	// Retaining the resolved identity is deliberate: future attempts cannot reuse
	// its UUID or create a second recovery delivery after config disable/re-enable.
	r.ExpectedVersion = st.Version
	r.Incident = nil
	st, err = s.CommitWatchdog(t.Context(), a, r)
	if err != nil {
		t.Fatal(err)
	}
	r.ExpectedVersion = st.Version
	r.Status = domain.ProbeWatchdogStarting
	r.Checkpoint.Armed = true
	st, err = s.CommitWatchdog(t.Context(), a, r)
	if err != nil {
		t.Fatal(err)
	}
	r.ExpectedVersion = st.Version
	r.Status = domain.ProbeWatchdogHealthy
	st, err = s.CommitWatchdog(t.Context(), a, r)
	if err != nil {
		t.Fatal(err)
	}
	r = watchdogOpening()
	r.ExpectedVersion = st.Version
	r.DeliveryIntents = nil
	if _, err := s.CommitWatchdog(t.Context(), a, r); err == nil {
		t.Fatal("reopened prior UUID")
	}
	r.Incident.SourceAlertID = "fc451959-0179-4465-b25f-410581f13c73"
	st, err = s.CommitWatchdog(t.Context(), a, r)
	if err != nil || st.IncidentSeq != 3 || st.Incident.SourceAlertID != r.Incident.SourceAlertID {
		t.Fatalf("new outage: %+v %v", st, err)
	}
	if n, err := s.db.NewSelect().Table("edge_delivery_outbox").Count(t.Context()); err != nil || n != 1 {
		t.Fatalf("disable/re-enable sent work: %d %v", n, err)
	}
}

func TestEdgeWatchdogFinalCounterFailureRollsBack(t *testing.T) {
	s, _, a := watchdogFixture(t)
	if _, err := s.db.ExecContext(t.Context(), "CREATE TRIGGER fail_counter BEFORE UPDATE OF last_created_seq ON edge_identity WHEN NEW.last_created_seq <> OLD.last_created_seq BEGIN SELECT RAISE(ABORT, 'counter fault'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitWatchdog(t.Context(), a, watchdogOpening()); err == nil {
		t.Fatal("counter failure accepted")
	}
	requireWatchdogEmpty(t, s)
}
