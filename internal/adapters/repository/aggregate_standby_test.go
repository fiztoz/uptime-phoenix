package repository_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

const standbyOwner = "88888888-8888-4888-8888-888888888888"

type standbyFixture struct {
	p     reviewPipeline
	store *repository.AggregateStandbyStore
	svc   *services.AggregateStandbyService
}
type standbyIncidentRow struct {
	IncidentID, Status, CloseReason       string
	AssignmentRevision, TransitionVersion int64
	OpenedAt                              time.Time
	ClosedAt                              *time.Time
}
type standbyPlanRow struct {
	IntentID, IncidentID, ChannelSHA256, Kind, Status, Reason string
	TransitionVersion, NotificationID                         int64
	CreatedAt                                                 time.Time
}

func newStandbyFixture(t *testing.T, engine string, mode domain.AlertDelivery) standbyFixture {
	t.Helper()
	p := newReviewPipeline(t, engine, false)
	seedInstallation(t, p.f)
	replaceBindingMode(t, p.f, p.monitor.ID, 1, domain.LocalProbeID, mode)
	if _, err := p.f.db.ExecContext(t.Context(), "UPDATE monitors SET resend_interval=1 WHERE id=?", p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	store := repository.NewAggregateStandbyStore(p.f.db)
	if _, err := services.NewAggregateStandbyService(store, standbyOwner).ProcessBatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	return standbyFixture{p: p, store: store, svc: services.NewAggregateStandbyService(store, standbyOwner)}
}
func (s standbyFixture) claim(t *testing.T, ttl time.Duration) domain.AggregateStandbyLease {
	t.Helper()
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET lease_until=?,due_at=? WHERE monitor_id=?", time.Now().UTC().Add(-time.Minute), time.Now().UTC().Add(-time.Minute), s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	cs, err := s.store.ClaimStandby(t.Context(), standbyOwner, ttl, 20)
	if err != nil || len(cs) != 1 {
		t.Fatal("claim did not select current standby work", err, len(cs))
	}
	return cs[0]
}
func (s standbyFixture) run(t *testing.T) domain.AggregateStandbyResult {
	t.Helper()
	got, err := s.store.ReconcileStandby(t.Context(), s.claim(t, 30*time.Second), s.svc)
	if err != nil {
		t.Fatal("standby reconcile", err)
	}
	return got
}
func (s standbyFixture) incidents(t *testing.T) []standbyIncidentRow {
	t.Helper()
	var rows []standbyIncidentRow
	if err := s.p.f.db.NewSelect().Table("aggregate_standby_incidents").Column("incident_id", "status", "close_reason", "assignment_revision", "transition_version", "opened_at", "closed_at").Where("monitor_id=?", s.p.monitor.ID).Order("transition_version ASC").Scan(t.Context(), &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}
func (s standbyFixture) plans(t *testing.T) []standbyPlanRow {
	t.Helper()
	var rows []standbyPlanRow
	if err := s.p.f.db.NewSelect().Table("aggregate_standby_intents").Column("intent_id", "incident_id", "channel_sha256", "kind", "status", "reason", "transition_version", "notification_id", "created_at").Order("transition_version ASC").Scan(t.Context(), &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}
func TestAggregateStandbyContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryRegional, domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
				t.Run(string(mode), func(t *testing.T) { testStandbyMode(t, engine, mode) })
			}
			for _, tc := range []struct {
				name string
				fn   func(*testing.T, standbyFixture)
			}{
				{"ColdPlanningDeadline", testStandbyCold}, {"EvidenceFilters", testStandbyFilters}, {"AdministrativeABA", testStandbyABA},
				{"ChannelIdentity", testStandbyChannel}, {"AtomicRollback", testStandbyRollback}, {"LeaseFences", testStandbyFences},
				{"ExpiryDuringEvaluation", testStandbyExpiry}, {"LockWaitExpiry", testStandbyLockWait}, {"ConcurrentWorkers", testStandbyConcurrent},
				{"Migration", testStandbyMigration}, {"WorkerBeforeAssignment", testStandbyWorker},
				{"UnknownHolds", testStandbyUnknownHolds}, {"CorruptStandbyRegional", testStandbyCorruptRegional}, {"ConcurrentStateWriter", testStandbyWriter},
				{"ClockDomains", testStandbyClockDomains}, {"RepeatSettings", testStandbyRepeatSettings}, {"IndependentBatch", testStandbyIndependentBatch},
			} {
				t.Run(tc.name, func(t *testing.T) { tc.fn(t, newStandbyFixture(t, engine, domain.AlertDeliveryBoth)) })
			}
			t.Run("CurrentAfterReplay", func(t *testing.T) { testStandbyReplay(t, engine) })
		})
	}
}
func testStandbyMode(t *testing.T, engine string, mode domain.AlertDelivery) {
	s := newStandbyFixture(t, engine, mode)
	s.record(t, domain.StatusDown)
	s.p.send(t)
	if len(s.p.sender.alerts) != 1 || s.p.sender.alerts[0].DeliveryScope == domain.IncidentScopeAggregate {
		t.Fatal("regional DOWN changed")
	}
	if mode == domain.AlertDeliveryRegional {
		cs, err := s.store.ClaimStandby(t.Context(), standbyOwner, time.Second, 20)
		if err != nil || len(cs) != 0 {
			t.Fatal("regional mode created aggregate work", err)
		}
	} else {
		const revision int64 = 9007199254740993
		if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE monitor_probe_assignment_sets SET revision=? WHERE monitor_id=?", revision, s.p.monitor.ID); err != nil {
			t.Fatal(err)
		}
		if got := s.run(t); got.Action != domain.AggregatePagingHold {
			t.Fatal("new revision reused old evidence")
		}
		s.record(t, domain.StatusDown)
		got := s.run(t)
		if got.Action != domain.AggregatePagingOpen || got.Health.Status != domain.StatusDown {
			t.Fatal("fresh desired mode did not open standby", got)
		}
		ps := s.plans(t)
		if len(ps) != 1 || ps[0].Status != "held" || ps[0].Kind != "down" || s.incidents(t)[0].AssignmentRevision != revision {
			t.Fatal("no exact held standby plan")
		}
		cs, err := s.p.outbox.ClaimDeliveries(t.Context(), domain.LocalProbeID, time.Now().UTC(), time.Minute, 20)
		if err != nil || len(cs) != 0 {
			t.Fatal("held plans entered source outbox", err)
		}
		s.record(t, domain.StatusUp)
		got = s.run(t)
		if got.Action != domain.AggregatePagingRecover {
			t.Fatal("fresh recovery not committed")
		}
		ps = s.plans(t)
		if len(ps) != 2 || ps[0].Status != "invalidated" || ps[1].Kind != "recovery" || ps[1].Status != "held" || s.incidents(t)[0].Status != "resolved" {
			t.Fatal("recovery fabricated live work")
		}
	}
	s.record(t, domain.StatusUp)
	s.p.send(t)
	if len(s.p.sender.alerts) != 2 || s.p.sender.alerts[1].Status != domain.StatusUp {
		t.Fatal("standby affected regional recovery", len(s.p.sender.alerts))
	}
}
func testStandbyCold(t *testing.T, s standbyFixture) {
	s.record(t, domain.StatusDown)
	first := s.run(t)
	before := s.plans(t)
	db := reopenConfigDB(t, s.p.f)
	s.store = repository.NewAggregateStandbyStore(db)
	s.svc = services.NewAggregateStandbyService(s.store, standbyOwner)
	if got := s.run(t); got.Action != domain.AggregatePagingHold || got.IncidentID != first.IncidentID || got.TransitionVersion != first.TransitionVersion {
		t.Fatal("restart replayed DOWN", got)
	}
	if got := s.plans(t); len(got) != 1 || got[0] != before[0] {
		t.Fatal("retry changed immutable held plan")
	}
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET last_planned_at=? WHERE monitor_id=?", time.Now().UTC().Add(-2*time.Minute), s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	next := s.run(t)
	if next.IncidentID != first.IncidentID || next.TransitionVersion != first.TransitionVersion+1 {
		t.Fatal("persisted planning deadline ignored")
	}
	plans := s.plans(t)
	if len(plans) != 2 || plans[0].Status != "invalidated" || plans[1].Kind != "repeat" || plans[1].Status != "held" {
		t.Fatal("planning repeat did not supersede held candidate")
	}
	s.run(t)
	if len(s.plans(t)) != 2 {
		t.Fatal("restart throttle allowed duplicate repeat")
	}
}
func testStandbyFilters(t *testing.T, s standbyFixture) {
	s.record(t, domain.StatusDown)
	for _, tc := range []struct {
		name, sql string
		value     any
	}{
		{"stale", "observed_at", time.Now().UTC().Add(-time.Hour)}, {"future", "observed_at", time.Now().UTC().Add(time.Hour)},
		{"generation", "assignment_generation", int64(99)}, {"stream", "stream_id", probeRegistryID3},
		{"pending", "status", int(domain.StatusPending)}, {"maintenance", "status", int(domain.StatusMaintenance)},
	} {
		s.record(t, domain.StatusDown)
		if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE monitor_probe_state SET "+tc.sql+"=? WHERE monitor_id=? AND probe_id=?", tc.value, s.p.monitor.ID, domain.LocalProbeID); err != nil {
			t.Fatal(err)
		}
		if got := s.run(t); got.Action != domain.AggregatePagingHold || len(s.incidents(t)) != 0 {
			t.Fatal(tc.name, "opened false incident", got)
		}
		// Reset the deliberate current-state corruption through the real recorder.
		if _, err := s.p.f.db.ExecContext(t.Context(), "DELETE FROM monitor_probe_state WHERE monitor_id=? AND probe_id=?", s.p.monitor.ID, domain.LocalProbeID); err != nil {
			t.Fatal(err)
		}
	}
	s.record(t, domain.StatusDown)
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE monitors SET active=? WHERE id=?", false, s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	if got := s.run(t); got.Action != domain.AggregatePagingHold || got.Health.Counts.Paused != 1 {
		t.Fatal("paused evidence became outage")
	}
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE monitors SET active=? WHERE id=?", true, s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	r := s.p.f.remote(t, probeRegistryID1, "standby-offline")
	set, err := s.p.f.assignments.GetByMonitorID(t.Context(), s.p.monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.NewProbeAssignmentStore(s.p.f.db).ReplaceWithBindings(t.Context(), s.p.monitor.ID, set.Revision, []string{domain.LocalProbeID, r.ID}, domain.HealthPolicyAllDown, domain.AlertDeliveryBoth, nil); err != nil {
		t.Fatal(err)
	}
	s.run(t) // Adopt ALL revision before collecting a new check.
	s.record(t, domain.StatusDown)
	if got := s.run(t); got.Action != domain.AggregatePagingHold || got.Health.Counts.Assigned != 2 || got.Health.Counts.Unknown != 1 {
		t.Fatal("offline source shrank ALL quorum", got)
	}
	set, err = s.p.f.assignments.GetByMonitorID(t.Context(), s.p.monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.p.f.assignments.Replace(t.Context(), s.p.monitor.ID, set.Revision, []string{domain.LocalProbeID, r.ID}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	s.run(t) // Adopt ANY revision on the DB clock.
	s.record(t, domain.StatusDown)
	if got := s.run(t); got.Action != domain.AggregatePagingOpen || got.Health.Counts.Unknown != 1 {
		t.Fatal("ANY down did not retain full membership", got)
	}
}
func testStandbyABA(t *testing.T, s standbyFixture) {
	s.record(t, domain.StatusDown)
	first := s.run(t)
	set, err := s.p.f.assignments.GetByMonitorID(t.Context(), s.p.monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.p.f.assignments.Replace(t.Context(), s.p.monitor.ID, set.Revision, []string{domain.LocalProbeID}, domain.HealthPolicyAllDown); err != nil {
		t.Fatal(err)
	}
	if _, err := s.p.f.assignments.Replace(t.Context(), s.p.monitor.ID, set.Revision+1, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	s.record(t, domain.StatusDown)
	closed := s.run(t)
	if closed.Action != domain.AggregatePagingAdminClose || len(s.incidents(t)) != 1 || s.incidents(t)[0].CloseReason != "assignment_revision_changed" || s.plans(t)[0].Reason != "administrative" {
		t.Fatal("ABA recovered/reopened an obsolete incident", closed)
	}
	s.record(t, domain.StatusDown) // Evidence after DB adoption/administrative close.
	reopened := s.run(t)
	if reopened.Action != domain.AggregatePagingOpen || reopened.IncidentID == first.IncidentID {
		t.Fatal("post-boundary fresh standby did not reopen separately")
	}
	set, err = s.p.f.assignments.GetByMonitorID(t.Context(), s.p.monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	replaceBindingMode(t, s.p.f, s.p.monitor.ID, set.Revision, domain.LocalProbeID, domain.AlertDeliveryRegional)
	if got := s.run(t); got.Action != domain.AggregatePagingAdminClose || s.incidents(t)[1].CloseReason != "delivery_mode_changed" {
		t.Fatal("desired mode change not administrative", got)
	}
	cs, err := s.store.ClaimStandby(t.Context(), standbyOwner, time.Second, 20)
	if err != nil || len(cs) != 0 {
		t.Fatal("closed regional monitor retained standby work", err)
	}
}
func testStandbyChannel(t *testing.T, s standbyFixture) {
	s.record(t, domain.StatusDown)
	s.run(t)
	before := s.plans(t)[0]
	// Same-second update proves digest identity is not a timestamp counter.
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE notifications SET config=? WHERE id=?", `{"url":"https://changed.example"}`, before.NotificationID); err != nil {
		t.Fatal(err)
	}
	s.run(t)
	ps := s.plans(t)
	if len(ps) != 1 || ps[0].Status != "invalidated" || ps[0].Reason != "channel_changed" {
		t.Fatal("old channel candidate survived config mutation")
	}
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET last_planned_at=? WHERE monitor_id=?", time.Now().UTC().Add(-2*time.Minute), s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	s.run(t)
	ps = s.plans(t)
	if len(ps) != 2 || ps[1].ChannelSHA256 == before.ChannelSHA256 {
		t.Fatal("channel digest not exact")
	}
	if _, err := s.p.f.db.ExecContext(t.Context(), "DELETE FROM monitor_notification WHERE monitor_id=?", s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	s.run(t)
	if s.plans(t)[1].Status != "invalidated" {
		t.Fatal("unlinked channel retained held plan")
	}
}
func testStandbyRollback(t *testing.T, s standbyFixture) {
	s.record(t, domain.StatusDown)
	claim := s.claim(t, 30*time.Second)
	trigger := "CREATE TRIGGER fail_standby BEFORE INSERT ON aggregate_standby_intents BEGIN SELECT RAISE(ABORT,'private standby fault'); END"
	if s.p.f.engine == "mariadb" {
		trigger = "CREATE TRIGGER fail_standby BEFORE INSERT ON aggregate_standby_intents FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='private standby fault'"
	}
	if _, err := s.p.f.db.ExecContext(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.p.f.db.ExecContext(context.Background(), "DROP TRIGGER IF EXISTS fail_standby") })
	_, err := s.store.ReconcileStandby(t.Context(), claim, s.svc)
	if !errors.Is(err, domain.ErrInternal) || strings.Contains(err.Error(), "private") {
		t.Fatal("storage fault not redacted", err)
	}
	if len(s.incidents(t)) != 0 || len(s.plans(t)) != 0 {
		t.Fatal("incident survived failed plan write")
	}
	if _, err := s.p.f.db.ExecContext(t.Context(), "DROP TRIGGER fail_standby"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.store.ReconcileStandby(t.Context(), claim, s.svc); err != nil || got.TransitionVersion != 1 {
		t.Fatal("failed transaction consumed version/claim", err)
	}
}
func testStandbyFences(t *testing.T, s standbyFixture) {
	s.record(t, domain.StatusDown)
	claim := s.claim(t, 30*time.Second)
	for _, bad := range []domain.AggregateStandbyLease{{MonitorID: claim.MonitorID, OwnerID: probeRegistryID3, Epoch: claim.Epoch, Until: claim.Until}, {MonitorID: claim.MonitorID, OwnerID: claim.OwnerID, Epoch: claim.Epoch + 1, Until: claim.Until}, {MonitorID: claim.MonitorID, OwnerID: claim.OwnerID, Epoch: claim.Epoch, Until: claim.Until.Add(time.Second)}} {
		if _, err := s.store.ReconcileStandby(t.Context(), bad, s.svc); !errors.Is(err, ports.ErrConflict) {
			t.Fatal("stale claim authorized write", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.store.ReconcileStandby(ctx, claim, s.svc); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET lease_until=? WHERE monitor_id=?", time.Now().UTC().Add(-time.Second), claim.MonitorID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.ReconcileStandby(t.Context(), claim, s.svc); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("expired claim wrote", err)
	}
	newer := s.claim(t, 30*time.Second)
	if newer.Epoch <= claim.Epoch {
		t.Fatal("expired same owner did not advance epoch")
	}
	if _, err := s.store.ReconcileStandby(t.Context(), claim, s.svc); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("old epoch matched replacement", err)
	}
	if len(s.incidents(t)) != 0 {
		t.Fatal("refused claim had effects")
	}
	if _, err := s.store.ReconcileStandby(t.Context(), newer, s.svc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET evaluated_at=? WHERE monitor_id=?", time.Now().UTC().Add(time.Hour), claim.MonitorID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.ReconcileStandby(t.Context(), s.claim(t, 30*time.Second), s.svc); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("clock rollback advanced history", err)
	}
}

type standbyBlockingEvaluator struct{ entered, release chan struct{} }

func (e standbyBlockingEvaluator) EvaluateStandby(in domain.AggregatePagingInput) (domain.AggregatePagingDecision, error) {
	close(e.entered)
	<-e.release
	return services.EvaluateAggregatePaging(in)
}
func testStandbyExpiry(t *testing.T, s standbyFixture) {
	s.record(t, domain.StatusDown)
	claim := s.claim(t, time.Second)
	e := standbyBlockingEvaluator{make(chan struct{}), make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := s.store.ReconcileStandby(t.Context(), claim, e); done <- err }()
	select {
	case <-e.entered:
	case <-time.After(3 * time.Second):
		close(e.release)
		<-done
		t.Fatal("evaluation not entered")
	}
	time.Sleep(1200 * time.Millisecond)
	close(e.release)
	if err := <-done; !errors.Is(err, ports.ErrConflict) {
		t.Fatal("lease expired inside transaction but committed", err)
	}
	if len(s.incidents(t)) != 0 || len(s.plans(t)) != 0 {
		t.Fatal("expired transaction committed effects")
	}
}
func testStandbyLockWait(t *testing.T, s standbyFixture) {
	s.record(t, domain.StatusDown)
	claim := s.claim(t, time.Second)
	tx, err := s.p.f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET lease_epoch=lease_epoch WHERE monitor_id=?", claim.MonitorID); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() { close(started); _, err := s.store.ReconcileStandby(t.Context(), claim, s.svc); done <- err }()
	<-started
	time.Sleep(1200 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ports.ErrConflict) {
		t.Fatal("clock sampled before lock wait", err)
	}
	if len(s.incidents(t)) != 0 {
		t.Fatal("lock-wait-expired claim created incident")
	}
}
func testStandbyConcurrent(t *testing.T, s standbyFixture) {
	s.record(t, domain.StatusDown)
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET due_at=? WHERE monitor_id=?", time.Now().UTC().Add(-time.Minute), s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	cs := make(chan []domain.AggregateStandbyLease, 2)
	errs := make(chan error, 2)
	start := make(chan struct{})
	for _, owner := range []string{standbyOwner, probeRegistryID3} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			<-start
			c, err := s.store.ClaimStandby(t.Context(), owner, 30*time.Second, 20)
			cs <- c
			errs <- err
		}(owner)
	}
	close(start)
	wg.Wait()
	close(cs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("competing claim", err)
		}
	}
	var claims []domain.AggregateStandbyLease
	for c := range cs {
		claims = append(claims, c...)
	}
	if len(claims) != 1 {
		t.Fatal("concurrent owners both acquired", len(claims))
	}
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := s.store.ReconcileStandby(t.Context(), claims[0], s.svc); done <- err }()
	}
	failures := 0
	for i := 0; i < 2; i++ {
		err := <-done
		if err != nil {
			if !errors.Is(err, ports.ErrConflict) {
				t.Fatal(err)
			}
			failures++
		}
	}
	if failures != 1 || len(s.incidents(t)) != 1 || len(s.plans(t)) != 1 {
		t.Fatal("duplicate reconciliation split incident/plan")
	}
}
func testStandbyMigration(t *testing.T, s standbyFixture) {
	s.record(t, domain.StatusDown)
	first := s.run(t)
	plan := s.plans(t)[0]
	for _, status := range []string{"pending", "leased", "sent"} {
		if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_intents SET status=? WHERE intent_id=?", status, plan.IntentID); err == nil {
			t.Fatal("schema permitted live provider status", status)
		}
	}
	if _, err := s.p.f.db.ExecContext(t.Context(), "INSERT INTO aggregate_standby_incidents SELECT ?,hub_id,monitor_id,open_monitor_id,assignment_revision,health_policy,alert_delivery,transition_version,status,opened_at,closed_at,close_reason FROM aggregate_standby_incidents WHERE incident_id=?", probeRegistryID3, first.IncidentID); err == nil {
		t.Fatal("two open standby incidents accepted")
	}
	if err := runEngineMigration(t, s.p.f.db, s.p.f.engine, "081_aggregate_standby", "down"); err != nil {
		t.Fatal(err)
	}
	if err := runEngineMigration(t, s.p.f.db, s.p.f.engine, "081_aggregate_standby", "up"); err != nil {
		t.Fatal(err)
	}
	s.run(t) // New adopted boundary; downgrade must not copy old eligibility.
	s.record(t, domain.StatusDown)
	next := s.run(t)
	if next.IncidentID == first.IncidentID || s.plans(t)[0].IntentID == plan.IntentID {
		t.Fatal("destructive re-up reused identities")
	}
	if _, err := s.p.f.db.ExecContext(t.Context(), "DELETE FROM monitors WHERE id=?", s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"aggregate_standby_state", "aggregate_standby_incidents", "aggregate_standby_intents"} {
		if replayCount(t, s.p.f, table) != 0 {
			t.Fatal("monitor deletion retained standby state", table)
		}
	}
}
func testStandbyWorker(t *testing.T, s standbyFixture) {
	// Existing worker starts before the desired aggregate assignment is enrolled.
	set, err := s.p.f.assignments.GetByMonitorID(t.Context(), s.p.monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	replaceBindingMode(t, s.p.f, s.p.monitor.ID, set.Revision, domain.LocalProbeID, domain.AlertDeliveryRegional)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	errs := make(chan error, 16)
	go func() {
		defer close(done)
		s.svc.Run(ctx, 10*time.Millisecond, func(e error) {
			select {
			case errs <- e:
			default:
			}
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	time.Sleep(40 * time.Millisecond)
	if len(s.incidents(t)) != 0 {
		t.Fatal("worker fabricated regional standby")
	}
	set, err = s.p.f.assignments.GetByMonitorID(t.Context(), s.p.monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	replaceBindingMode(t, s.p.f, s.p.monitor.ID, set.Revision, domain.LocalProbeID, domain.AlertDeliveryBoth)
	s.record(t, domain.StatusDown)
	// A first legitimate UNKNOWN capture may precede recording and schedule
	// the next production reconciliation five seconds later. Wait beyond that
	// bounded due interval rather than racing its exact deadline.
	deadline := time.Now().Add(12 * time.Second)
	for len(s.incidents(t)) == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		s.record(t, domain.StatusDown) // Eventually post-adoption, still a real heartbeat.
	}
	cancel()
	<-done
	// Replace cleanup's join channel with a closed marker: the worker is already joined.
	done = make(chan struct{})
	close(done)
	if len(s.incidents(t)) != 1 || len(s.plans(t)) != 1 {
		t.Fatal("running worker did not discover desired assignment")
	}
	select {
	case e := <-errs:
		t.Fatal("worker reported error", e)
	default:
	}
	s.p.send(t)
	if len(s.p.sender.alerts) != 1 {
		t.Fatal("standby worker suppressed regional provider")
	}
}
func testStandbyReplay(t *testing.T, engine string) {
	r := newReplayFixture(t, engine)
	set, err := r.f.assignments.GetByMonitorID(t.Context(), r.monitor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.NewProbeAssignmentStore(r.f.db).ReplaceWithBindings(t.Context(), r.monitor, set.Revision, []string{r.session.ProbeID}, set.HealthPolicy, domain.AlertDeliveryBoth, nil); err != nil {
		t.Fatal(err)
	}
	store := repository.NewAggregateStandbyStore(r.f.db)
	svc := services.NewAggregateStandbyService(store, standbyOwner)
	run := func() domain.AggregateStandbyResult {
		t.Helper()
		if _, err := r.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET lease_until=?,due_at=? WHERE monitor_id=?", time.Now().UTC().Add(-time.Minute), time.Now().UTC().Add(-time.Minute), r.monitor); err != nil {
			t.Fatal(err)
		}
		cs, err := store.ClaimStandby(t.Context(), standbyOwner, 30*time.Second, 20)
		if err != nil || len(cs) != 1 {
			t.Fatal(err)
		}
		got, err := store.ReconcileStandby(t.Context(), cs[0], svc)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	obs := func(seq int64, status domain.Status, at time.Time) domain.ProbeReplayEvent {
		e := r.observation(seq)
		e.ObservedAt = at
		e.Observation.ObservedAt = at
		e.Observation.Status = status
		e.Observation.RawStatus = status
		return e
	}
	old := time.Now().UTC().Add(-time.Hour)
	r.ingest(t, r.batch(obs(1, domain.StatusDown, old), obs(2, domain.StatusUp, old.Add(time.Second)), obs(3, domain.StatusDown, old.Add(2*time.Second))))
	if replayCount(t, r.f, "aggregate_standby_incidents") != 0 || run().Action != domain.AggregatePagingHold {
		t.Fatal("historical replay emitted standby transitions")
	}
	var boundary time.Time
	if err := r.f.db.NewRaw("SELECT policy_effective_at FROM aggregate_standby_state WHERE monitor_id=?", r.monitor).Scan(t.Context(), &boundary); err != nil {
		t.Fatal(err)
	}
	for time.Now().UTC().Before(boundary.Add(time.Millisecond)) {
		time.Sleep(time.Millisecond)
	}
	r.ingest(t, r.batch(obs(4, domain.StatusDown, time.Now().UTC())))
	if run().Action != domain.AggregatePagingOpen {
		t.Fatal("fresh current snapshot after committed replay did not open standby")
	}
	r.ingest(t, r.batch(obs(5, domain.StatusUp, time.Now().UTC().Add(time.Hour))))
	if run().Action != domain.AggregatePagingHold {
		t.Fatal("future source time created recovery")
	}
	r.ingest(t, r.batch(obs(6, domain.StatusUp, time.Now().UTC())))
	if run().Action != domain.AggregatePagingRecover || replayCount(t, r.f, "probe_delivery_intents") != 0 {
		t.Fatal("current recovery created source provider work")
	}
}
