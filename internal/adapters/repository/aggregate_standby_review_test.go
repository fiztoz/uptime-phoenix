package repository_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func testStandbyClockDomains(t *testing.T, s standbyFixture) {
	s.record(t, domain.StatusDown)
	set, err := s.p.f.assignments.GetByMonitorID(t.Context(), s.p.monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.p.f.assignments.Replace(t.Context(), s.p.monitor.ID, set.Revision, []string{domain.LocalProbeID}, domain.HealthPolicyAllDown); err != nil {
		t.Fatal(err)
	}
	// A writer's wall clock ahead cannot error the DB-clock planner; its old
	// check must not become post-adoption evidence under the new revision.
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE monitor_probe_assignment_sets SET updated_at=? WHERE monitor_id=?", time.Now().UTC().Add(time.Hour), s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	if got := s.run(t); got.Action != domain.AggregatePagingHold || got.Health.Counts.Unknown != 1 {
		t.Fatal("process-clock boundary supplied paging truth", got)
	}
	s.record(t, domain.StatusDown)
	if got := s.run(t); got.Action != domain.AggregatePagingOpen {
		t.Fatal("DB-adopted fresh evidence refused", got)
	}
	set, err = s.p.f.assignments.GetByMonitorID(t.Context(), s.p.monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.p.f.assignments.Replace(t.Context(), s.p.monitor.ID, set.Revision, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE monitor_probe_assignment_sets SET updated_at=? WHERE monitor_id=?", time.Now().UTC().Add(-time.Hour), s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	if got := s.run(t); got.Action != domain.AggregatePagingAdminClose {
		t.Fatal("new revision not administrative", got)
	}
	if got := s.run(t); got.Action != domain.AggregatePagingHold || got.Health.Counts.Unknown != 1 {
		t.Fatal("old evidence replayed after behind clock", got)
	}
	s.record(t, domain.StatusDown)
	if got := s.run(t); got.Action != domain.AggregatePagingOpen {
		t.Fatal("new evidence not admitted", got)
	}
}
func testStandbyRepeatSettings(t *testing.T, s standbyFixture) {
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE monitors SET resend_interval=0 WHERE id=?", s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	s.record(t, domain.StatusDown)
	s.run(t)
	var next *time.Time
	if err := s.p.f.db.NewRaw("SELECT next_repeat_at FROM aggregate_standby_state WHERE monitor_id=?", s.p.monitor.ID).Scan(t.Context(), &next); err != nil || next != nil {
		t.Fatal("zero interval armed planning", err)
	}
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE monitors SET resend_interval=1 WHERE id=?", s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	s.run(t)
	if err := s.p.f.db.NewRaw("SELECT next_repeat_at FROM aggregate_standby_state WHERE monitor_id=?", s.p.monitor.ID).Scan(t.Context(), &next); err != nil || next == nil || len(s.plans(t)) != 1 {
		t.Fatal("enabled repeat deadline not restored", err)
	}
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET last_planned_at=? WHERE monitor_id=?", time.Now().UTC().Add(-2*time.Minute), s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	s.run(t)
	if len(s.plans(t)) != 2 || s.plans(t)[1].Kind != "repeat" {
		t.Fatal("live interval change lost repeat")
	}
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE monitors SET resend_interval=0 WHERE id=?", s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	s.run(t)
	next = nil // Bun does not clear an already-populated pointer when SQL is NULL.
	if err := s.p.f.db.NewRaw("SELECT next_repeat_at FROM aggregate_standby_state WHERE monitor_id=?", s.p.monitor.ID).Scan(t.Context(), &next); err != nil || next != nil {
		t.Fatal("disabled repeat retained deadline", err)
	}
}
func testStandbyIndependentBatch(t *testing.T, s standbyFixture) {
	id := localMonitor(t, s.p.f)
	set, err := s.p.f.assignments.GetByMonitorID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	replaceBindingMode(t, s.p.f, id, set.Revision, domain.LocalProbeID, domain.AlertDeliveryBoth)
	// Capture its DB adoption boundary before recording a current check.
	if _, err := s.svc.ProcessBatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	monitor, err := s.p.repos.MonitorRepo.GetByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var boundary time.Time
	if err := s.p.f.db.NewRaw("SELECT policy_effective_at FROM aggregate_standby_state WHERE monitor_id=?", id).Scan(t.Context(), &boundary); err != nil {
		t.Fatal(err)
	}
	for !time.Now().UTC().After(boundary.Add(time.Millisecond)) {
		time.Sleep(time.Millisecond)
	}
	if err := s.p.heartbeats.Record(t.Context(), monitor, ports.CheckResult{Status: domain.StatusDown, AssignmentGeneration: 1, ConfigRevision: 1}); err != nil {
		t.Fatal(err)
	}
	awaitProbeClockFor(t, s.p.f.db, id)
	// First monitor has valid lease storage but an invalid reconciliation binding.
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET assignment_revision=? WHERE monitor_id=?", int64(999), s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Minute)
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET due_at=?,lease_until=?", past, past); err != nil {
		t.Fatal(err)
	}
	n, err := s.svc.ProcessBatch(t.Context())
	if n != 1 || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("bad reconciliation stranded healthy claims", n, err)
	}
	var open int
	if err := s.p.f.db.NewRaw("SELECT COUNT(*) FROM aggregate_standby_incidents WHERE open_monitor_id=?", id).Scan(t.Context(), &open); err != nil || open != 1 {
		t.Fatal("healthy monitor had no independent effect", err)
	}
	// A corrupt/exhausted claim must likewise preserve independently committed
	// healthy claims rather than returning a fictitious all-or-nothing batch.
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET lease_epoch=? WHERE monitor_id=?", int64(math.MaxInt64), s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET due_at=?,lease_until=?", past, past); err != nil {
		t.Fatal(err)
	}
	claims, err := repository.NewAggregateStandbyStore(s.p.f.db).ClaimStandby(t.Context(), standbyOwner, 30*time.Second, 20)
	if len(claims) != 1 || claims[0].MonitorID != id || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("bad claim rolled back healthy acquisition", len(claims), err)
	}
	if _, err := s.store.ReconcileStandby(t.Context(), claims[0], s.svc); err != nil {
		t.Fatal("partial committed claim was not authoritative", err)
	}
}
