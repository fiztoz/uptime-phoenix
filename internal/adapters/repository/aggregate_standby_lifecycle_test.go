package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func testStandbyUnknownHolds(t *testing.T, s standbyFixture) {
	s.record(t, domain.StatusDown)
	first := s.run(t)
	for _, status := range []domain.Status{domain.StatusPending, domain.StatusMaintenance, domain.StatusUnknown} {
		if status == domain.StatusUnknown {
			if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE monitor_probe_state SET observed_at=? WHERE monitor_id=?", time.Now().UTC().Add(-time.Hour), s.p.monitor.ID); err != nil {
				t.Fatal(err)
			}
		} else {
			s.record(t, status)
		}
		got := s.run(t)
		if got.Action != domain.AggregatePagingHold || got.IncidentID != first.IncidentID || got.TransitionVersion != first.TransitionVersion || s.incidents(t)[0].Status != "open" || len(s.plans(t)) != 1 {
			t.Fatal("non-UP evidence fabricated recovery", got)
		}
	}
}
func testStandbyCorruptRegional(t *testing.T, s standbyFixture) {
	s.record(t, domain.StatusDown)
	s.run(t)
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET hub_id=? WHERE monitor_id=?", probeRegistryID3, s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.ClaimStandby(t.Context(), standbyOwner, time.Second, 20); err != nil && !errors.Is(err, ports.ErrConflict) {
		t.Fatal("unexpected failure", err)
	}
	// An independently captured current claim must refuse corrupted installation
	// identity too, rather than using an earlier in-memory lease as authenticity.
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET hub_id=? WHERE monitor_id=?", "11111111-2222-4333-8444-555555555555", s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	claim := s.claim(t, 30*time.Second)
	if _, err := s.p.f.db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET hub_id=? WHERE monitor_id=?", probeRegistryID3, s.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.ReconcileStandby(t.Context(), claim, s.svc); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stored foreign installation authorized effects", err)
	}
	s.record(t, domain.StatusUp)
	s.p.send(t)
	if len(s.p.sender.alerts) != 2 || s.p.sender.alerts[0].Status != domain.StatusDown || s.p.sender.alerts[1].Status != domain.StatusUp {
		t.Fatal("standby corruption entered regional delivery")
	}
}
func testStandbyWriter(t *testing.T, s standbyFixture) {
	s.record(t, domain.StatusDown)
	claim := s.claim(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	e := standbyBlockingEvaluator{make(chan struct{}), make(chan struct{})}
	reader := make(chan error, 1)
	writer := make(chan error, 1)
	go func() { _, err := s.store.ReconcileStandby(ctx, claim, e); reader <- err }()
	select {
	case <-e.entered:
	case <-ctx.Done():
		close(e.release)
		<-reader
		t.Fatal("reader did not capture current snapshot")
	}
	go func() {
		writer <- s.p.heartbeats.Record(ctx, s.p.monitor, ports.CheckResult{Status: domain.StatusUp, Message: "concurrent recovery", AssignmentGeneration: 1, ConfigRevision: 1})
	}()
	// Commit source state cannot interleave through the snapshot transaction.
	select {
	case err := <-writer:
		close(e.release)
		<-reader
		t.Fatal("writer passed captured-state locks", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(e.release)
	if err := <-reader; err != nil {
		t.Fatal("serialized reader", err)
	}
	if err := <-writer; err != nil {
		t.Fatal("source writer", err)
	}
	if len(s.incidents(t)) != 1 || s.incidents(t)[0].Status != "open" {
		t.Fatal("captured DOWN not committed atomically")
	}
	if got := s.run(t); got.Action != domain.AggregatePagingRecover {
		t.Fatal("next snapshot did not see committed recovery", got)
	}
}

func (s standbyFixture) record(t *testing.T, status domain.Status) {
	t.Helper()
	var boundary *time.Time
	err := s.p.f.db.NewRaw("SELECT policy_effective_at FROM aggregate_standby_state WHERE monitor_id=?", s.p.monitor.ID).Scan(t.Context(), &boundary)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if boundary != nil {
		deadline := time.Now().Add(3 * time.Second)
		for !time.Now().UTC().After(boundary.UTC().Add(time.Millisecond)) {
			if time.Now().After(deadline) {
				t.Fatal("source clock has not reached DB adoption boundary")
			}
			time.Sleep(time.Millisecond)
		}
	}
	s.p.record(t, status)
	awaitProbeClock(t, s.p)
}
