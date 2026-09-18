package services

import (
	"context"
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type alertAssignmentReader struct {
	ports.MonitorProbeAssignmentRepository
	set *domain.MonitorProbeAssignments
	err error
}

func (r alertAssignmentReader) GetByMonitorID(context.Context, int64) (*domain.MonitorProbeAssignments, error) {
	return r.set, r.err
}

func TestDispatcherAssignmentGuardsBeforeSideEffects(t *testing.T) {
	ctx := context.Background()
	for name, repo := range map[string]alertAssignmentReader{
		"storage error":                  {err: errors.New("unavailable")},
		"missing set for new generation": {err: ports.ErrNotFound},
		"nil set":                        {},
		"remote only":                    {set: &domain.MonitorProbeAssignments{Assignments: []domain.ProbeAssignment{{ProbeID: "remote", Generation: 2}}}},
		"old generation":                 {set: &domain.MonitorProbeAssignments{Assignments: []domain.ProbeAssignment{{ProbeID: "local", Generation: 3}}}},
	} {
		t.Run(name, func(t *testing.T) {
			notifier, groups, throttle := &fakeNotifier{}, &throttleGroupSpy{}, &throttleSpy{allowed: true}
			d := NewNotificationDispatcher(notifier, &fakeMaintenance{})
			d.SetAssignmentRepository(repo)
			d.SetGroupEvaluator(groups)
			d.SetThrottleRepository(throttle)
			hb := &domain.Heartbeat{MonitorID: 1, ProbeID: "local", AssignmentGeneration: 2, Status: domain.StatusDown}
			d.OnHeartbeat(ctx, &domain.Monitor{ID: 1}, hb, nil)
			if notifier.count() != 0 || groups.calls != 0 || len(throttle.calls) != 0 || len(throttle.cleared) != 0 {
				t.Fatal("rejected assignment caused side effects")
			}
		})
	}
}

func TestEscalationAssignmentLookupFailureKeepsLadderRetryable(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 0, 100))
	if err := h.assign.AssignMonitor(context.Background(), m.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	a := h.openFiringAlert(t, m)
	h.svc.SetAssignmentRepository(alertAssignmentReader{err: errors.New("temporary database failure")})
	if got := h.runDue(t); got != 0 || h.notifier.count() != 0 {
		t.Fatal("failed assignment lookup delivered")
	}
	e, err := h.state.GetByAlertID(context.Background(), a.ID)
	if err != nil || e.Status != domain.EscalationStatePending {
		t.Fatal("temporary error canceled the ladder")
	}
}

func TestEscalationRejectsForeignOrInactiveIncidentAtStart(t *testing.T) {
	h := newEscHarness(t)
	m := h.monitor(nil)
	p := h.policies.seed(true, step(1, 0, 100))
	if err := h.assign.AssignMonitor(context.Background(), m.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	for _, a := range []*domain.Alert{
		{ID: 99, MonitorID: m.ID + 1, Status: domain.AlertStatusFiring},
		{ID: 99, MonitorID: m.ID, Status: domain.AlertStatusAcked},
		{ID: 99, MonitorID: m.ID, Status: domain.AlertStatusFiring, ProbeID: "11111111-1111-4111-8111-111111111111", AssignmentGeneration: 1},
		{ID: 99, MonitorID: m.ID, Status: domain.AlertStatusFiring, ProbeID: "local", AssignmentGeneration: -1},
	} {
		if err := h.svc.StartForAlert(context.Background(), a, m); err == nil {
			t.Fatal("invalid incident started a ladder")
		}
	}
	if len(h.state.byID) != 0 {
		t.Fatal("rejected start persisted state")
	}
}
