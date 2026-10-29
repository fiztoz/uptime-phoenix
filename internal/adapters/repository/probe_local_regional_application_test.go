package repository_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TestLocalRegionalApplicationContract exercises source-local state through real
// activation, cold execution reads, heartbeat transactions and provider work.
// MariaDB leaves require a disposable server; their skips are not acceptance.
func TestLocalRegionalApplicationContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryRegional, domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
				t.Run(string(mode), func(t *testing.T) { testLocalRegionalMode(t, engine, mode) })
			}
			for _, tc := range []struct {
				name string
				run  func(*testing.T, probeRegistryFixture)
			}{
				{"ActivationRollback", testLocalRegionalRollback},
				{"RetryAndUpgrade", testLocalRegionalRetryUpgrade},
				{"PartialUpgrade", testLocalRegionalPartialUpgrade},
				{"RecordingFence", testLocalRegionalRecording},
				{"Migration", testLocalRegionalMigration},
			} {
				t.Run(tc.name, func(t *testing.T) { tc.run(t, newProbeRegistryFixture(t, engine)) })
			}
			for _, column := range []string{"assignment_revision", "generation", "hub_id", "sha256", "monitor_id", "stream_id", "alert_delivery"} {
				t.Run("PresentMismatch/"+column, func(t *testing.T) { testLocalRegionalMismatch(t, newProbeRegistryFixture(t, engine), column) })
			}
			t.Run("OutboxBeforeSend", func(t *testing.T) { testLocalRegionalBeforeSend(t, engine) })
			t.Run("EscalationEnqueue", func(t *testing.T) { testLocalRegionalEscalation(t, engine) })
		})
	}
}

func localRegionalRead(t *testing.T, f probeRegistryFixture, cold bool) *domain.LocalProbeConfigDefinition {
	t.Helper()
	db := f.db
	if cold {
		db = reopenConfigDB(t, f)
	}
	got, err := activationRepo(f, db).(ports.LocalAppliedConfigReader).ReadAppliedLocal(t.Context())
	if err != nil || got == nil {
		t.Fatal("read local execution graph", err)
	}
	return got
}

func assertLocalRegionalFact(t *testing.T, got *domain.LocalProbeConfigDefinition, monitor int64, meta domain.ProbeConfigMetadata, binding int64) {
	t.Helper()
	if len(got.RegionalDeliveryApplications) != 1 {
		t.Fatal("missing/duplicate regional application")
	}
	want := domain.ProbeDeliveryModeApplication{ProbeConfigTarget: localDeliveryTarget(), MonitorID: monitor,
		AssignmentRevision: binding, Generation: 1, ConfigRevision: meta.Revision, SHA256: meta.SHA256,
		StreamID: domain.LocalStreamID, AlertDelivery: domain.AlertDeliveryRegional}
	if got.RegionalDeliveryApplications[0] != want {
		t.Fatal("source-local application tuple changed")
	}
}

func testLocalRegionalMode(t *testing.T, engine string, mode domain.AlertDelivery) {
	p := newReviewPipeline(t, engine, true)
	set, err := p.f.assignments.GetByMonitorID(t.Context(), p.monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	replaceBindingMode(t, p.f, p.monitor.ID, set.Revision, domain.LocalProbeID, mode)
	const binding int64 = 9007199254740993
	if _, err := p.f.db.ExecContext(t.Context(), "UPDATE monitor_probe_assignment_sets SET revision = ? WHERE monitor_id = ?", binding, p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	meta := prepareLocalDeliveryRevision(t, p.f, p.f.db, 1, time.Now().UTC())
	activateLocalDelivery(t, p.f, p.f.db, meta, 1)
	got := localRegionalRead(t, p.f, true)
	assertLocalRegionalFact(t, got, p.monitor.ID, meta, binding)
	if got.Assignments[0].AlertDelivery != mode {
		t.Fatal("desired document mode changed to effective regional")
	}
	// The new facts must participate in real recording and final pre-send reads,
	// while aggregate/both desire still sends regional DOWN and recovery.
	for _, status := range []domain.Status{domain.StatusDown, domain.StatusUp} {
		if err := p.heartbeats.Record(t.Context(), p.monitor, ports.CheckResult{Status: status,
			Message: "source-local regional", AssignmentGeneration: 1, ConfigRevision: meta.Revision}); err != nil {
			t.Fatal(err)
		}
		p.send(t)
	}
	if len(p.sender.alerts) != 2 || p.sender.alerts[0].Status != domain.StatusDown || p.sender.alerts[1].Status != domain.StatusUp {
		t.Fatal("desired mode suppressed or changed regional DOWN/recovery")
	}
	assertTableCount(t, p.f, "probe_local_regional_applications", 2) // Retained old/new documents.
}

func testLocalRegionalRollback(t *testing.T, f probeRegistryFixture) {
	monitor := seedLocalDelivery(t, f)
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	remove := injectOutboxFailure(t, f, "probe_local_regional_applications", "INSERT")
	got, err := activationRepo(f, f.db).ActivateLocal(t.Context(), ports.LocalActivationParams{Target: meta.ProbeConfigTarget,
		Revision: meta.Revision, SHA256: meta.SHA256, AppliedAt: time.Now().UTC(), AssignmentCount: -1})
	remove()
	if got != nil || err == nil {
		t.Fatal("injected application write failure committed activation")
	}
	for _, table := range []string{"probe_active_configs", "probe_config_applied_receipts", "probe_local_regional_applications"} {
		assertTableCount(t, f, table, 0)
	}
	// Source freshness rejects the candidate before any mode state can persist.
	replaceBindingMode(t, f, monitor, 1, domain.LocalProbeID, domain.AlertDeliveryBoth)
	if got, err := activationRepo(f, f.db).ActivateLocal(t.Context(), ports.LocalActivationParams{Target: meta.ProbeConfigTarget,
		Revision: meta.Revision, SHA256: meta.SHA256, AppliedAt: time.Now().UTC(), AssignmentCount: -1}); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stale candidate acquired regional application state", err)
	}
	assertTableCount(t, f, "probe_local_regional_applications", 0)
}

func testLocalRegionalRetryUpgrade(t *testing.T, f probeRegistryFixture) {
	monitor := seedLocalDelivery(t, f)
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	activateLocalDelivery(t, f, f.db, meta, 0)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := activationRepo(f, f.db).(ports.LocalAppliedConfigReader).ReadAppliedLocal(canceled); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled execution read ignored cancellation", err)
	}
	assertTableCount(t, f, "probe_local_regional_applications", 1)
	before := localRegionalRead(t, f, false)
	active, err := activationRepo(f, f.db).GetActive(t.Context(), domain.LocalProbeID)
	if err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err := f.db.NewSelect().Table("probe_config_snapshots").Column("protected_payload").Where("probe_id = ? AND revision = 1", domain.LocalProbeID).Scan(t.Context(), &payload); err != nil {
		t.Fatal(err)
	}
	activateLocalDelivery(t, f, f.db, meta, 0)
	if !reflect.DeepEqual(localRegionalRead(t, f, false).RegionalDeliveryApplications, before.RegionalDeliveryApplications) {
		t.Fatal("same-document retry changed facts")
	}
	// Explicit bound pre-upgrade fixture: existing document/receipt, no mode rows.
	if _, err := f.db.ExecContext(t.Context(), "DELETE FROM probe_local_regional_applications"); err != nil {
		t.Fatal(err)
	}
	activateLocalDelivery(t, f, f.db, meta, 0)
	if len(localRegionalRead(t, f, true).RegionalDeliveryApplications) != 0 {
		t.Fatal("receipt retry or execution read fabricated missing application facts")
	}
	assertTableCount(t, f, "probe_local_regional_applications", 0)
	current, err := refreshService(t, f, f.db).Refresh(t.Context())
	if err != nil || current.Revision != 2 {
		t.Fatal("pre-upgrade document blocked normal fresh activation", err)
	}
	secondMeta := domain.ProbeConfigMetadata{ProbeConfigTarget: current.ProbeConfigTarget, Revision: current.Revision, SHA256: current.SHA256}
	assertLocalRegionalFact(t, localRegionalRead(t, f, true), monitor, secondMeta, 1)
	assertTableCount(t, f, "probe_local_regional_applications", 1)
	old, err := activationRepo(f, f.db).GetReceipt(t.Context(), domain.LocalProbeID, 1)
	if err != nil || !domain.SameProbeActiveConfig(*old, *active) {
		t.Fatal("refresh rewrote old receipt", err)
	}
	var retained []byte
	if err := f.db.NewSelect().Table("probe_config_snapshots").Column("protected_payload").Where("probe_id = ? AND revision = 1", domain.LocalProbeID).Scan(t.Context(), &retained); err != nil || !reflect.DeepEqual(payload, retained) {
		t.Fatal("refresh rewrote protected document", err)
	}
	again, err := refreshService(t, f, f.db).Refresh(t.Context())
	if err != nil || again.Revision != 2 {
		t.Fatal("complete application state did not satisfy refresh fast path", err)
	}
}

func testLocalRegionalPartialUpgrade(t *testing.T, f probeRegistryFixture) {
	first := seedLocalDelivery(t, f)
	second := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	activateLocalDelivery(t, f, f.db, meta, 0)
	assertTableCount(t, f, "probe_local_regional_applications", 2)
	if _, err := f.db.ExecContext(t.Context(), "DELETE FROM probe_local_regional_applications WHERE monitor_id = ?", second); err != nil {
		t.Fatal(err)
	}
	got := localRegionalRead(t, f, true)
	if len(got.Assignments) != 2 || len(got.RegionalDeliveryApplications) != 1 || got.RegionalDeliveryApplications[0].MonitorID != first {
		t.Fatal("partial evidence shrank membership or fabricated a missing member")
	}
	current, err := refreshService(t, f, f.db).Refresh(t.Context())
	if err != nil || current == nil || current.Revision != 2 {
		t.Fatal("partial application state blocked fresh upgrade", err)
	}
	got = localRegionalRead(t, f, true)
	if len(got.Assignments) != 2 || len(got.RegionalDeliveryApplications) != 2 {
		t.Fatal("new revision did not restore complete truthful regional facts")
	}
	assertTableCount(t, f, "probe_local_regional_applications", 3) // Old surviving fact is unchanged.
}

func testLocalRegionalMismatch(t *testing.T, f probeRegistryFixture, column string) {
	monitor := seedLocalDelivery(t, f)
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	activateLocalDelivery(t, f, f.db, meta, 0)
	value := any(int64(2)) // Fault values intentionally span integer and text columns.
	switch column {
	case "hub_id":
		value = probeRegistryID3
	case "sha256":
		value = strings.Repeat("0", 64)
	case "monitor_id":
		value = monitor + 1000
	case "stream_id":
		value = probeRegistryID3
	case "alert_delivery":
		value = domain.AlertDeliveryAggregate
	}
	// Deliberately damaged storage, not a supported write. Restore constraint
	// checks on this one disposable connection before exercising real reads.
	if column == "stream_id" || column == "alert_delivery" {
		f.db.SetMaxOpenConns(1)
		disable := "PRAGMA ignore_check_constraints = ON"
		if f.engine == "mariadb" {
			disable = "SET SESSION check_constraint_checks = OFF"
		}
		if _, err := f.db.ExecContext(t.Context(), disable); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.NewUpdate().Table("probe_local_regional_applications").Set(column+" = ?", value).Where("probe_id = ?", domain.LocalProbeID).Exec(t.Context()); err != nil {
		t.Fatal(err)
	}
	if column == "stream_id" || column == "alert_delivery" {
		enable := "PRAGMA ignore_check_constraints = OFF"
		if f.engine == "mariadb" {
			enable = "SET SESSION check_constraint_checks = ON"
		}
		if _, err := f.db.ExecContext(t.Context(), enable); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := activationRepo(f, f.db).(ports.LocalAppliedConfigReader).ReadAppliedLocal(t.Context()); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("present mismatched application did not refuse execution", err)
	}
	// Retry never repairs or derives facts from current desired data.
	activateLocalDelivery(t, f, f.db, meta, 0)
	if got, err := activationRepo(f, reopenConfigDB(t, f)).(ports.LocalAppliedConfigReader).ReadAppliedLocal(t.Context()); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("retry repaired corrupted application", err)
	}
	// Receipt-backed diagnostics remain exact document-only reads.
	if readLocalDelivery(t, newLocalDeliveryStore(f.db, nil, localDeliveryProtector(t))).Revision != meta.Revision {
		t.Fatal("application state changed independent document diagnostic")
	}
}

func testLocalRegionalRecording(t *testing.T, f probeRegistryFixture) {
	monitor := seedLocalDelivery(t, f)
	fence := claimFence(t, f, monitor, "local-regional")
	commit := fencedDownCommit(t, f, monitor, fence)
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	activateLocalDelivery(t, f, f.db, meta, 0)
	before := fenceSideEffects(t, f, monitor)
	if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_local_regional_applications SET generation = 2"); err != nil {
		t.Fatal(err)
	}
	if got, err := f.localHeartbeat.CommitLocalHeartbeat(t.Context(), commit); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("recording bypassed corrupt application", err)
	}
	assertNoFenceSideEffects(t, f, monitor, before)
	if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_local_regional_applications SET generation = 1"); err != nil {
		t.Fatal(err)
	}
	// A policy ABA changes only the set revision; the old document/fact remains
	// unchanged and must not authorize newly bound protected recording.
	changed, err := repository.NewProbeAssignmentStore(f.db).ReplaceWithBindings(t.Context(), monitor, 1, []string{domain.LocalProbeID}, domain.HealthPolicyAllDown, domain.AlertDeliveryRegional, nil)
	if err != nil || changed.Revision != 2 {
		t.Fatal("policy change did not commit a new set binding", err)
	}
	returned := replaceBindingMode(t, f, monitor, 2, domain.LocalProbeID, domain.AlertDeliveryRegional)
	if returned.Revision != 3 || returned.Assignments[0].Generation != 1 {
		t.Fatal("policy ABA did not preserve generation and advance set binding")
	}
	if got, err := f.localHeartbeat.CommitLocalHeartbeat(t.Context(), commit); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("recording ignored stored set binding", err)
	}
	assertNoFenceSideEffects(t, f, monitor, before)
	fresh := prepareLocalDeliveryRevision(t, f, f.db, 1, time.Now().UTC())
	activateLocalDelivery(t, f, f.db, fresh, 1)
	commit.Heartbeat.ConfigRevision = fresh.Revision
	commit.DeliveryIntents[0].NotificationVersion = fresh.Revision
	if got, err := f.localHeartbeat.CommitLocalHeartbeat(t.Context(), commit); err != nil || got == nil || got.SourceSeq != 1 {
		t.Fatal("current protected recording did not commit", err)
	}
	assertTableCount(t, f, "probe_delivery_intents", 1)
	assertTableCount(t, f, "heartbeats", 1)
}

func testLocalRegionalBeforeSend(t *testing.T, engine string) {
	p := newReviewPipeline(t, engine, true)
	p.record(t, domain.StatusDown)
	assertTableCount(t, p.f, "probe_delivery_intents", 1)
	var deliveryID string
	if err := p.f.db.NewSelect().Table("probe_delivery_intents").Column("delivery_id").Scan(t.Context(), &deliveryID); err != nil {
		t.Fatal(err)
	}
	original, err := p.outbox.GetDeliveryIntent(t.Context(), domain.LocalProbeID, deliveryID)
	if err != nil || original == nil {
		t.Fatal("read original intent", err)
	}
	var generation int64
	if err := p.f.db.NewSelect().Table("probe_local_regional_applications").Column("generation").Scan(t.Context(), &generation); err != nil {
		t.Fatal(err)
	}
	if _, err := p.f.db.ExecContext(t.Context(), "UPDATE probe_local_regional_applications SET generation = ?", generation+1); err != nil {
		t.Fatal(err)
	}
	p.send(t)
	if len(p.sender.alerts) != 0 {
		t.Fatal("pre-send execution read bypassed present corrupt application")
	}
	queued, err := p.outbox.GetDeliveryIntent(t.Context(), domain.LocalProbeID, deliveryID)
	if err != nil || queued == nil || queued.Status != domain.DeliveryStatusLeased || queued.LeaseToken == "" {
		t.Fatal("unreadable application discarded queued work instead of retaining its claim", err)
	}
	if queued.NotificationVersion != original.NotificationVersion || queued.ConfigRevision != original.ConfigRevision {
		t.Fatal("refusal rewrote original replay identity")
	}
	// Simulate repair of the persisted application evidence, not a consumer-side
	// repair. The same still-owned claim may retry; refusal must not lose the alert.
	if _, err := p.f.db.ExecContext(t.Context(), "UPDATE probe_local_regional_applications SET generation = ?", generation); err != nil {
		t.Fatal(err)
	}
	if err := p.consumer.ProcessOne(t.Context(), *queued); err != nil {
		t.Fatal("retry after application repair", err)
	}
	stored, err := p.outbox.GetDeliveryIntent(t.Context(), domain.LocalProbeID, deliveryID)
	if err != nil || stored == nil || stored.Status != domain.DeliveryStatusSent || len(p.sender.alerts) != 1 {
		t.Fatal("repaired application did not send the retained intent exactly once in this test", err)
	}
	if stored.NotificationVersion != original.NotificationVersion || stored.ConfigRevision != original.ConfigRevision {
		t.Fatal("successful retry rewrote original replay identity")
	}
	assertTableCount(t, p.f, "probe_delivery_intents", 1)
}

func testLocalRegionalEscalation(t *testing.T, engine string) {
	p, svc, policy, revision := dueEscalationPipeline(t, engine)
	assertTableCount(t, p.f, "probe_delivery_intents", 1)
	if _, err := p.f.db.ExecContext(t.Context(), "UPDATE probe_local_regional_applications SET generation = 2"); err != nil {
		t.Fatal(err)
	}
	if steps, err := svc.RunDue(t.Context()); err != nil || steps != 0 {
		t.Fatal("escalation runner advanced corrupt application", err)
	}
	assertTableCount(t, p.f, "probe_delivery_intents", 1)
	if len(p.sender.alerts) != 0 {
		t.Fatal("escalation performed provider I/O")
	}
	// Direct writer also refuses inside its transaction, independent of runner.
	now := time.Now().UTC().Add(3 * time.Minute) // The runner's failed claim expires after two minutes.
	rows, err := p.repos.AlertEscalationRepo.ClaimDue(t.Context(), "local-step", now, now.Add(time.Minute))
	if err != nil || len(rows) != 1 {
		t.Fatal("claim pending step", err)
	}
	plan := domain.EscalationStepCommit{EscalationID: rows[0].ID, ClaimToken: "local-step", ExpectedStep: 1,
		ConfigRevision: revision, At: now, NotificationIDs: policy.Steps[0].NotificationIDs, NextStep: 2, NextRunAt: now.Add(time.Minute)}
	writer := repository.NewEscalationOutboxStore(p.f.db, probe.LocalConfigEncoder{})
	if ok, err := writer.CommitEscalationStep(t.Context(), plan); ok || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("escalation transaction bypassed present corrupt application", err)
	}
	assertTableCount(t, p.f, "probe_delivery_intents", 1)
	if _, err := p.f.db.ExecContext(t.Context(), "UPDATE probe_local_regional_applications SET generation = 1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := writer.CommitEscalationStep(t.Context(), plan); err != nil || !ok {
		t.Fatal("otherwise identical valid escalation plan did not commit", err)
	}
	assertTableCount(t, p.f, "probe_delivery_intents", 2)
}

func testLocalRegionalMigration(t *testing.T, f probeRegistryFixture) {
	monitor := seedLocalDelivery(t, f)
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	activateLocalDelivery(t, f, f.db, meta, 0)
	for _, tc := range []struct{ column, value string }{{"alert_delivery", "aggregate"}, {"stream_id", probeRegistryID3}, {"generation", "0"}, {"probe_id", probeRegistryID1}} {
		if _, err := f.db.NewUpdate().Table("probe_local_regional_applications").Set(tc.column+" = ?", tc.value).Where("probe_id = ?", domain.LocalProbeID).Exec(t.Context()); err == nil {
			t.Fatal("schema accepted invalid application", tc.column)
		}
	}
	assertLocalRegionalFact(t, localRegionalRead(t, f, false), monitor, meta, 1)
	// Storage-only cascade fixture: an unapplied retained snapshot may be deleted.
	// The copied row is not presented as production application evidence.
	second := prepareLocalDeliveryRevision(t, f, f.db, 1, time.Now().UTC())
	if _, err := f.db.ExecContext(t.Context(), "INSERT INTO probe_local_regional_applications (probe_id, config_revision, monitor_id, assignment_revision, generation, hub_id, stream_id, sha256, alert_delivery) SELECT probe_id, ?, monitor_id, assignment_revision, generation, hub_id, stream_id, ?, alert_delivery FROM probe_local_regional_applications WHERE config_revision = 1", second.Revision, second.SHA256); err != nil {
		t.Fatal(err)
	}
	assertTableCount(t, f, "probe_local_regional_applications", 2)
	if _, err := f.db.ExecContext(t.Context(), "DELETE FROM probe_config_snapshots WHERE probe_id = ? AND revision = ?", domain.LocalProbeID, second.Revision); err != nil {
		t.Fatal(err)
	}
	assertTableCount(t, f, "probe_local_regional_applications", 1)
	if err := runEngineMigration(t, f.db, f.engine, "077_local_regional_application", "down"); err != nil {
		t.Fatal(err)
	}
	if err := runEngineMigration(t, f.db, f.engine, "077_local_regional_application", "up"); err != nil {
		t.Fatal(err)
	}
	if len(localRegionalRead(t, f, false).RegionalDeliveryApplications) != 0 {
		t.Fatal("migration fabricated a backfill")
	}
	// No affected snapshot is deleted while active/receipt references exist.
	assertTableCount(t, f, "probe_active_configs", 1)
	assertTableCount(t, f, "probe_config_applied_receipts", 1)
}
