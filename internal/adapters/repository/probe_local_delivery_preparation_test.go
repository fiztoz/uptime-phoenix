package repository_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TestLocalDeliveryPreparationContract uses real publication, protected local
// activation, cold reads, production refresh and (for modes) regional provider work.
func TestLocalDeliveryPreparationContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryRegional, domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
				t.Run(string(mode), func(t *testing.T) { testLocalPreparationMode(t, engine, mode) })
			}
			for _, tc := range []struct {
				name string
				run  func(*testing.T, probeRegistryFixture)
			}{
				{"AtomicRollback", testLocalPreparationRollback}, {"RetryAndColdUpgrade", testLocalPreparationUpgrade},
				{"PartialUpgrade", testLocalPreparationPartial}, {"ModeAndPolicyABA", testLocalPreparationABA},
				{"PrerequisitesAndCancellation", testLocalPreparationPrerequisites}, {"ConcurrentSelection", testLocalPreparationConcurrent},
				{"Migration", testLocalPreparationMigration}, {"WorkerBeforeAssignment", testLocalPreparationWorker},
			} {
				t.Run(tc.name, func(t *testing.T) { tc.run(t, newProbeRegistryFixture(t, engine)) })
			}
			for _, column := range []string{"assignment_revision", "generation", "hub_id", "sha256", "monitor_id", "stream_id", "alert_delivery"} {
				t.Run("PresentMismatch/"+column, func(t *testing.T) { testLocalPreparationMismatch(t, newProbeRegistryFixture(t, engine), column) })
			}
		})
	}
}

func localPreparationRead(t *testing.T, f probeRegistryFixture, cold bool, active domain.ProbeActiveConfig) []domain.LocalDeliveryPreparation {
	t.Helper()
	db := f.db
	if cold {
		db = reopenConfigDB(t, f)
	}
	got, err := activationRepo(f, db).(ports.LocalDeliveryPreparationReader).ReadLocalDeliveryPreparations(t.Context(), active)
	if err != nil {
		t.Fatal("read local pending requests", err)
	}
	return got
}
func localPreparationActive(t *testing.T, f probeRegistryFixture) domain.ProbeActiveConfig {
	t.Helper()
	got, err := activationRepo(f, f.db).GetActive(t.Context(), domain.LocalProbeID)
	if err != nil || got == nil {
		t.Fatal("active configuration", err)
	}
	return *got
}
func seedLocalPreparation(t *testing.T, f probeRegistryFixture) int64 {
	t.Helper()
	monitor := seedLocalDelivery(t, f)
	replaceBindingMode(t, f, monitor, 1, domain.LocalProbeID, domain.AlertDeliveryAggregate)
	return monitor
}
func assertLocalPreparation(t *testing.T, p domain.LocalDeliveryPreparation, monitor, binding int64, active domain.ProbeActiveConfig, mode domain.AlertDelivery) {
	t.Helper()
	want := domain.ProbeDeliveryModeApplication{ProbeConfigTarget: active.ProbeConfigTarget, MonitorID: monitor, AssignmentRevision: binding, Generation: 1, ConfigRevision: active.Revision, SHA256: active.SHA256, StreamID: domain.LocalStreamID, AlertDelivery: mode}
	if p.ProbeDeliveryModeApplication != want || !domain.ValidLocalDeliveryPreparation(p) {
		t.Fatal("pending exact tuple changed")
	}
}
func testLocalPreparationMode(t *testing.T, engine string, mode domain.AlertDelivery) {
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
	active := localPreparationActive(t, p.f)
	ps := localPreparationRead(t, p.f, true, active)
	if mode == domain.AlertDeliveryRegional {
		if len(ps) != 0 {
			t.Fatal("regional member fabricated preparation")
		}
	} else {
		if len(ps) != 1 {
			t.Fatal("missing local pending request")
		}
		assertLocalPreparation(t, ps[0], p.monitor.ID, binding, active, mode)
	}
	assertLocalRegionalFact(t, localRegionalRead(t, p.f, false), p.monitor.ID, meta, binding)
	for _, status := range []domain.Status{domain.StatusDown, domain.StatusUp} {
		if err := p.heartbeats.Record(t.Context(), p.monitor, ports.CheckResult{Status: status, Message: "local pending request", AssignmentGeneration: 1, ConfigRevision: meta.Revision}); err != nil {
			t.Fatal(err)
		}
		p.send(t)
	}
	if len(p.sender.alerts) != 2 || p.sender.alerts[0].Status != domain.StatusDown || p.sender.alerts[1].Status != domain.StatusUp {
		t.Fatal("pending request changed regional DOWN/recovery")
	}
}
func testLocalPreparationRollback(t *testing.T, f probeRegistryFixture) {
	monitor := seedLocalPreparation(t, f)
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	remove := injectOutboxFailure(t, f, "probe_local_delivery_preparations", "INSERT")
	got, err := activationRepo(f, f.db).ActivateLocal(t.Context(), ports.LocalActivationParams{Target: meta.ProbeConfigTarget, Revision: meta.Revision, SHA256: meta.SHA256, AssignmentCount: -1, AppliedAt: time.Now().UTC()})
	remove()
	if got != nil || err == nil {
		t.Fatal("failed preparation write committed activation")
	}
	for _, table := range []string{"probe_active_configs", "probe_config_applied_receipts", "probe_local_regional_applications", "probe_local_delivery_preparations"} {
		assertTableCount(t, f, table, 0)
	}
	replaceBindingMode(t, f, monitor, 2, domain.LocalProbeID, domain.AlertDeliveryBoth)
	if got, err := activationRepo(f, f.db).ActivateLocal(t.Context(), ports.LocalActivationParams{Target: meta.ProbeConfigTarget, Revision: meta.Revision, SHA256: meta.SHA256, AssignmentCount: -1, AppliedAt: time.Now().UTC()}); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stale source prepared local request", err)
	}
	assertTableCount(t, f, "probe_local_delivery_preparations", 0)
}
func testLocalPreparationUpgrade(t *testing.T, f probeRegistryFixture) {
	monitor := seedLocalPreparation(t, f)
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	activateLocalDelivery(t, f, f.db, meta, 0)
	active := localPreparationActive(t, f)
	before := localPreparationRead(t, f, false, active)
	activateLocalDelivery(t, f, f.db, meta, 0)
	if !reflect.DeepEqual(before, localPreparationRead(t, f, true, active)) {
		t.Fatal("same-document retry rewrote request/time")
	}
	var payload []byte
	if err := f.db.NewSelect().Table("probe_config_snapshots").Column("protected_payload").Where("probe_id = ? AND revision = 1", domain.LocalProbeID).Scan(t.Context(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(t.Context(), "DELETE FROM probe_local_delivery_preparations WHERE probe_id = ?", domain.LocalProbeID); err != nil {
		t.Fatal(err)
	}
	activateLocalDelivery(t, f, f.db, meta, 0)
	if len(localPreparationRead(t, f, true, active)) != 0 {
		t.Fatal("receipt retry backfilled local request")
	}
	current, err := refreshService(t, f, reopenConfigDB(t, f)).Refresh(t.Context())
	if err != nil || current == nil || current.Revision != 2 {
		t.Fatal("cold pre-upgrade source never got new request", err)
	}
	ps := localPreparationRead(t, f, true, *current)
	if len(ps) != 1 {
		t.Fatal("fresh upgrade missing request")
	}
	assertLocalPreparation(t, ps[0], monitor, 2, *current, domain.AlertDeliveryAggregate)
	old, err := activationRepo(f, f.db).GetReceipt(t.Context(), domain.LocalProbeID, 1)
	if err != nil || !domain.SameProbeActiveConfig(*old, active) {
		t.Fatal("upgrade changed retained receipt", err)
	}
	var retained []byte
	if err := f.db.NewSelect().Table("probe_config_snapshots").Column("protected_payload").Where("probe_id = ? AND revision = 1", domain.LocalProbeID).Scan(t.Context(), &retained); err != nil || !reflect.DeepEqual(payload, retained) {
		t.Fatal("upgrade changed retained protected bytes", err)
	}
	again, err := refreshService(t, f, f.db).Refresh(t.Context())
	if err != nil || again.Revision != 2 {
		t.Fatal("complete requests not stable on refresh", err)
	}
}
func testLocalPreparationPartial(t *testing.T, f probeRegistryFixture) {
	first := seedLocalPreparation(t, f)
	second := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	replaceBindingMode(t, f, second, 1, domain.LocalProbeID, domain.AlertDeliveryBoth)
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	activateLocalDelivery(t, f, f.db, meta, 0)
	active := localPreparationActive(t, f)
	if _, err := f.db.ExecContext(t.Context(), "DELETE FROM probe_local_delivery_preparations WHERE monitor_id = ?", second); err != nil {
		t.Fatal(err)
	}
	ps := localPreparationRead(t, f, true, active)
	if len(ps) != 1 || ps[0].MonitorID != first {
		t.Fatal("partial state shrank or fabricated membership")
	}
	current, err := refreshService(t, f, f.db).Refresh(t.Context())
	if err != nil || current.Revision != 2 {
		t.Fatal("partial state not upgraded", err)
	}
	if len(localPreparationRead(t, f, false, *current)) != 2 {
		t.Fatal("new document missing required local members")
	}
	assertTableCount(t, f, "probe_local_delivery_preparations", 3)
}
func testLocalPreparationABA(t *testing.T, f probeRegistryFixture) {
	monitor := seedLocalPreparation(t, f)
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	activateLocalDelivery(t, f, f.db, meta, 0)
	old := localPreparationActive(t, f)
	before := localPreparationRead(t, f, false, old)
	set, err := f.assignments.GetByMonitorID(t.Context(), monitor)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryBoth, domain.AlertDeliveryRegional, domain.AlertDeliveryAggregate} {
		replaceBindingMode(t, f, monitor, set.Revision, domain.LocalProbeID, mode)
		if got, err := activationRepo(f, f.db).(ports.LocalDeliveryPreparationReader).ReadLocalDeliveryPreparations(t.Context(), old); got != nil || !errors.Is(err, ports.ErrConflict) {
			t.Fatal("old document rebound to mutable desired rows", err)
		}
		active, err := refreshService(t, f, f.db).Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		ps := localPreparationRead(t, f, true, *active)
		if mode == domain.AlertDeliveryRegional {
			if len(ps) != 0 {
				t.Fatal("regional transition retained selected request")
			}
		} else if len(ps) != 1 || ps[0].AlertDelivery != mode {
			t.Fatal("mode transition failed local request")
		}
		set, err = f.assignments.GetByMonitorID(t.Context(), monitor)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Historical rows keep their original set binding even through mode ABA.
	var binding int64
	if err := f.db.NewSelect().Table("probe_local_delivery_preparations").Column("assignment_revision").Where("config_revision = 1 AND monitor_id = ?", monitor).Scan(t.Context(), &binding); err != nil || binding != before[0].AssignmentRevision {
		t.Fatal("historical request rebound", err)
	}
	active := localPreparationActive(t, f)
	if _, err := repository.NewProbeAssignmentStore(f.db).ReplaceWithBindings(t.Context(), monitor, set.Revision, []string{domain.LocalProbeID}, domain.HealthPolicyAllDown, domain.AlertDeliveryAggregate, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := activationRepo(f, f.db).(ports.LocalDeliveryPreparationReader).ReadLocalDeliveryPreparations(t.Context(), active); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("same-mode policy revision accepted old request", err)
	}
}
func testLocalPreparationPrerequisites(t *testing.T, f probeRegistryFixture) {
	seedLocalPreparation(t, f)
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	activateLocalDelivery(t, f, f.db, meta, 0)
	active := localPreparationActive(t, f)
	reader := activationRepo(f, f.db).(ports.LocalDeliveryPreparationReader)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := reader.ReadLocalDeliveryPreparations(canceled, active); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
	wrong := active
	wrong.Revision++
	if got, err := reader.ReadLocalDeliveryPreparations(t.Context(), wrong); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("foreign active tuple accepted", err)
	}
	if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_config_applied_receipts SET assignment_count = assignment_count + 1 WHERE probe_id = ?", domain.LocalProbeID); err != nil {
		t.Fatal(err)
	}
	if got, err := reader.ReadLocalDeliveryPreparations(t.Context(), active); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("committed exact receipt not required", err)
	}
	if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_config_applied_receipts SET assignment_count = assignment_count - 1 WHERE probe_id = ?", domain.LocalProbeID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(t.Context(), "DELETE FROM probe_local_regional_applications WHERE probe_id = ?", domain.LocalProbeID); err != nil {
		t.Fatal(err)
	}
	if got, err := reader.ReadLocalDeliveryPreparations(t.Context(), active); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("present request survived missing regional prerequisite", err)
	}
	// Even corrupt/missing preparation is not a new execution gate.
	if _, err := activationRepo(f, f.db).(ports.LocalAppliedConfigReader).ReadAppliedLocal(t.Context()); err != nil {
		t.Fatal("request rows changed regional execution", err)
	}
}
func testLocalPreparationMismatch(t *testing.T, f probeRegistryFixture, column string) {
	seedLocalPreparation(t, f)
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	activateLocalDelivery(t, f, f.db, meta, 0)
	active := localPreparationActive(t, f)
	value := any(int64(3))
	switch column {
	case "hub_id", "stream_id":
		value = probeRegistryID3
	case "sha256":
		value = strings.Repeat("0", 64)
	case "alert_delivery":
		value = string(domain.AlertDeliveryBoth)
	}
	if column == "stream_id" {
		f.db.SetMaxOpenConns(1)
		disable := "PRAGMA ignore_check_constraints = ON"
		if f.engine == "mariadb" {
			disable = "SET SESSION check_constraint_checks = OFF"
		}
		if _, err := f.db.ExecContext(t.Context(), disable); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.NewUpdate().Table("probe_local_delivery_preparations").Set(column+" = ?", value).Where("probe_id = ?", domain.LocalProbeID).Exec(t.Context()); err != nil {
		t.Fatal(err)
	}
	if column == "stream_id" {
		enable := "PRAGMA ignore_check_constraints = OFF"
		if f.engine == "mariadb" {
			enable = "SET SESSION check_constraint_checks = ON"
		}
		if _, err := f.db.ExecContext(t.Context(), enable); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := activationRepo(f, f.db).(ports.LocalDeliveryPreparationReader).ReadLocalDeliveryPreparations(t.Context(), active); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("present corrupt request accepted/partially returned", err)
	}
	// Retry must not repair it; the old successful activation receipt is unchanged.
	activateLocalDelivery(t, f, f.db, meta, 0)
	if _, err := refreshService(t, f, f.db).Refresh(t.Context()); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("refresher repaired present corrupt request", err)
	}
	if _, err := activationRepo(f, f.db).(ports.LocalAppliedConfigReader).ReadAppliedLocal(t.Context()); err != nil {
		t.Fatal("pending request became execution authority", err)
	}
	assertTableCount(t, f, "probe_config_snapshots", 1)
}

type localPreparationBlockingEncoder struct {
	once             sync.Once
	entered, release chan struct{}
	blockingRevision int64
}

func (e *localPreparationBlockingEncoder) EncodeLocal(d domain.LocalProbeConfigDefinition) ([]byte, error) {
	if d.Revision == e.blockingRevision {
		e.once.Do(func() { close(e.entered); <-e.release })
	}
	return (probe.LocalConfigEncoder{}).EncodeLocal(d)
}
func testLocalPreparationConcurrent(t *testing.T, f probeRegistryFixture) {
	seedLocalPreparation(t, f)
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	activateLocalDelivery(t, f, f.db, meta, 0)
	old := localPreparationActive(t, f)
	next := prepareLocalDeliveryRevision(t, f, f.db, 1, time.Now().UTC())
	encoder := &localPreparationBlockingEncoder{entered: make(chan struct{}), release: make(chan struct{}), blockingRevision: 1}
	release := sync.OnceFunc(func() { close(encoder.release) })
	reader := repository.NewProbeActivationStore(reopenConfigDB(t, f), encoder)
	writer := activationRepo(f, reopenConfigDB(t, f))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	readDone := make(chan error, 1)
	activateDone := make(chan error, 1)
	readJoined, activateJoined, activateStarted := false, false, false
	defer func() {
		release()
		cancel()
		if !readJoined {
			select {
			case _ = <-readDone:
			case <-time.After(5 * time.Second):
				t.Error("reader did not join after failure")
			}
		}
		if activateStarted && !activateJoined {
			select {
			case _ = <-activateDone:
			case <-time.After(5 * time.Second):
				t.Error("activation did not join after failure")
			}
		}
	}()
	go func() {
		ps, err := reader.ReadLocalDeliveryPreparations(ctx, old)
		if err == nil && (len(ps) != 1 || ps[0].ConfigRevision != 1) {
			err = errors.New("torn selected request")
		}
		readDone <- err
	}()
	select {
	case <-encoder.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not reach captured graph")
	}
	activateStarted = true
	go func() {
		_, err := writer.ActivateLocal(ctx, ports.LocalActivationParams{Target: next.ProbeConfigTarget, Revision: next.Revision, SHA256: next.SHA256, ExpectedActiveRevision: 1, AppliedAt: time.Now().UTC(), AssignmentCount: -1})
		activateDone <- err
	}()
	select {
	case err := <-activateDone:
		activateJoined = true
		t.Fatal("activation crossed locked selected graph", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	for i, done := range []chan error{readDone, activateDone} {
		select {
		case err := <-done:
			if i == 0 {
				readJoined = true
			} else {
				activateJoined = true
			}
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent transactions did not join")
		}
	}
	if got, err := reader.ReadLocalDeliveryPreparations(t.Context(), old); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("old reader target selected newer request", err)
	}
	active := localPreparationActive(t, f)
	ps := localPreparationRead(t, f, true, active)
	if len(ps) != 1 || ps[0].ConfigRevision != 2 {
		t.Fatal("new committed request missing")
	}
}
func testLocalPreparationWorker(t *testing.T, f probeRegistryFixture) {
	seedInstallation(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	refresh := refreshService(t, f, f.db)
	refresh.SetReconcileInterval(10 * time.Millisecond)
	refresh.StartEventSubscription(ctx, nil)
	defer func() { cancel(); _, _ = refresh.Refresh(ctx) }() // Drain in-flight mutex holder before DB cleanup.
	deadline := time.Now().Add(5 * time.Second)
	for {
		active, err := activationRepo(f, f.db).GetActive(t.Context(), domain.LocalProbeID)
		if err == nil && active.AssignmentCount == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cold worker did not activate empty pre-membership source", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	monitor := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(t.Context(), monitor); err != nil {
		t.Fatal(err)
	}
	replaceBindingMode(t, f, monitor, 1, domain.LocalProbeID, domain.AlertDeliveryBoth)
	deadline = time.Now().Add(5 * time.Second)
	for {
		active, err := activationRepo(f, f.db).GetActive(t.Context(), domain.LocalProbeID)
		if err == nil && active.AssignmentCount == 1 {
			ps, readErr := activationRepo(f, f.db).(ports.LocalDeliveryPreparationReader).ReadLocalDeliveryPreparations(t.Context(), *active)
			if readErr == nil && len(ps) == 1 {
				assertLocalPreparation(t, ps[0], monitor, 2, *active, domain.AlertDeliveryBoth)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("already-running worker never prepared newly assigned member", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testLocalPreparationMigration(t *testing.T, f probeRegistryFixture) {
	seedLocalPreparation(t, f)
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	activateLocalDelivery(t, f, f.db, meta, 0)
	active := localPreparationActive(t, f)
	for _, tc := range []struct {
		column string
		value  any
	}{{"generation", 0}, {"assignment_revision", 0}, {"alert_delivery", "regional"}, {"stream_id", probeRegistryID3}, {"probe_id", probeRegistryID1}, {"sha256", strings.Repeat("z", 64)}, {"prepared_at", nil}} {
		if _, err := f.db.NewUpdate().Table("probe_local_delivery_preparations").Set(tc.column+" = ?", tc.value).Where("probe_id = ?", domain.LocalProbeID).Exec(t.Context()); err == nil {
			t.Fatal("schema accepted invalid request", tc.column)
		}
	}
	// Storage-only retained snapshot proves THIS FK independently of active/receipt.
	next := prepareLocalDeliveryRevision(t, f, f.db, 1, time.Now().UTC())
	if _, err := f.db.ExecContext(t.Context(), "INSERT INTO probe_local_delivery_preparations (probe_id,config_revision,monitor_id,assignment_revision,generation,hub_id,stream_id,sha256,alert_delivery,prepared_at) SELECT probe_id,?,monitor_id,assignment_revision,generation,hub_id,stream_id,?,alert_delivery,prepared_at FROM probe_local_delivery_preparations WHERE config_revision = 1", next.Revision, next.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(t.Context(), "DELETE FROM probe_config_snapshots WHERE probe_id = ? AND revision = ?", domain.LocalProbeID, next.Revision); err == nil {
		t.Fatal("retained request lost its snapshot")
	}
	for _, direction := range []string{"down", "up"} {
		if err := runEngineMigration(t, f.db, f.engine, "079_local_delivery_preparation", direction); err != nil {
			t.Fatal(err)
		}
	}
	if len(localPreparationRead(t, f, true, active)) != 0 {
		t.Fatal("migration backfilled old request")
	}
	current, err := refreshService(t, f, f.db).Refresh(t.Context())
	if err != nil || current.Revision != 2 {
		t.Fatal("explicit downgrade/re-up blocked normal fresh activation", err)
	}
}
