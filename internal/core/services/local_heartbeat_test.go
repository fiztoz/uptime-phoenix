package services

import (
	"context"
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// This fake implements the atomic port, including rejected stale evaluations,
// rather than letting service tests silently fall back to two separate writes.
type fakeLocalHeartbeatRecorder struct {
	*fakeRegionalRepo
	heartbeats   *fakeHeartbeatRepo
	beforeCommit func()
	commitErr    error
	attempts     []domain.LocalHeartbeatCommit
}

func (r *fakeLocalHeartbeatRecorder) CommitLocalHeartbeat(ctx context.Context, commit domain.LocalHeartbeatCommit) (*domain.Heartbeat, error) {
	r.attempts = append(r.attempts, commit)
	if r.beforeCommit != nil {
		r.beforeCommit()
	}
	if r.commitErr != nil {
		return nil, r.commitErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	hb := commit.Heartbeat
	key := regionalStateKey(hb.MonitorID, hb.ProbeID)
	if r.states[key].Seq != commit.ExpectedStateSeq {
		return nil, ports.ErrStaleLocalState
	}
	hb.SourceSeq = int64(len(r.obs)) + 1
	if err := r.heartbeats.Save(ctx, &hb); err != nil {
		return nil, err
	}
	obs := domain.RegionalObservation{
		MonitorID: hb.MonitorID, ProbeID: hb.ProbeID, AssignmentGeneration: hb.AssignmentGeneration,
		StreamID: hb.StreamID, Seq: hb.SourceSeq, Status: hb.Status, RawStatus: commit.RawStatus,
		DownCount: hb.DownCount, ObservedAt: hb.Time, ReceivedAt: hb.ReceivedAt,
	}
	r.obs = append(r.obs, obs)
	r.states[key] = domain.RegionalState{MonitorID: hb.MonitorID, ProbeID: hb.ProbeID,
		AssignmentGeneration: hb.AssignmentGeneration, StreamID: hb.StreamID, Seq: hb.SourceSeq,
		Status: hb.Status, DownCount: hb.DownCount, ObservedAt: hb.Time, ReceivedAt: hb.ReceivedAt}
	return &hb, nil
}

func TestLocalHeartbeatRetriesStaleEvaluationBeforePublishing(t *testing.T) {
	ctx := context.Background()
	heartbeats, regional, bus := newFakeHeartbeatRepo(), newFakeRegionalRepo(), newFakeBus()
	recorder := &fakeLocalHeartbeatRecorder{fakeRegionalRepo: regional, heartbeats: heartbeats}
	svc := NewHeartbeatService(heartbeats, bus)
	svc.SetRegionalRecorder(nil, recorder)
	monitor := &domain.Monitor{ID: 1, MaxRetries: 1}
	recorder.beforeCommit = func() {
		recorder.beforeCommit = nil
		// A different worker confirms the first failure after our initial read.
		regional.states[regionalStateKey(1, domain.LocalProbeID)] = domain.RegionalState{
			MonitorID: 1, ProbeID: domain.LocalProbeID, AssignmentGeneration: 1,
			Seq: 1, Status: domain.StatusPending, DownCount: 1,
		}
		regional.obs = append(regional.obs, domain.RegionalObservation{Seq: 1})
	}
	if err := svc.Record(ctx, monitor, ports.CheckResult{Status: domain.StatusDown}); err != nil {
		t.Fatal(err)
	}
	if len(heartbeats.heartbeats) != 1 || heartbeats.heartbeats[0].SourceSeq != 2 || heartbeats.heartbeats[0].DownCount != 2 || heartbeats.heartbeats[0].Status != domain.StatusDown {
		t.Fatalf("stale evaluation was persisted: %+v", heartbeats.heartbeats)
	}
	if len(bus.events) != 2 {
		t.Fatalf("retry published intermediate hints: %+v", bus.events)
	}
	if len(recorder.attempts) != 2 || !recorder.attempts[0].Heartbeat.Time.Equal(recorder.attempts[1].Heartbeat.Time) || recorder.attempts[0].RawStatus != recorder.attempts[1].RawStatus {
		t.Fatal("retry changed the original check result or time")
	}
}

func TestLocalHeartbeatRetryCannotMoveResultToNewAssignment(t *testing.T) {
	ctx := context.Background()
	heartbeats, regional, bus := newFakeHeartbeatRepo(), newFakeRegionalRepo(), newFakeBus()
	assignments := healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{
		1: {MonitorID: 1, Assignments: []domain.ProbeAssignment{{MonitorID: 1, ProbeID: domain.LocalProbeID, Generation: 1}}},
	}}
	recorder := &fakeLocalHeartbeatRecorder{fakeRegionalRepo: regional, heartbeats: heartbeats, commitErr: ports.ErrStaleLocalState}
	recorder.beforeCommit = func() {
		assignments.sets[1].Assignments[0].Generation = 2
	}
	svc := NewHeartbeatService(heartbeats, bus)
	svc.SetRegionalRecorder(assignments, recorder)
	if err := svc.Record(ctx, &domain.Monitor{ID: 1}, ports.CheckResult{Status: domain.StatusDown}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("result was retargeted after assignment replacement: %v", err)
	}
	if len(recorder.attempts) != 1 || len(heartbeats.heartbeats) != 0 || len(bus.events) != 0 {
		t.Fatal("assignment replacement produced a retried write or hint")
	}
}

func TestLocalHeartbeatCommitFailurePublishesNothing(t *testing.T) {
	ctx := context.Background()
	heartbeats, regional, bus := newFakeHeartbeatRepo(), newFakeRegionalRepo(), newFakeBus()
	failure := errors.New("disk full")
	recorder := &fakeLocalHeartbeatRecorder{fakeRegionalRepo: regional, heartbeats: heartbeats, commitErr: failure}
	svc := NewHeartbeatService(heartbeats, bus)
	svc.SetRegionalRecorder(nil, recorder)
	if err := svc.Record(ctx, &domain.Monitor{ID: 1}, ports.CheckResult{Status: domain.StatusDown}); !errors.Is(err, failure) {
		t.Fatalf("record failure = %v", err)
	}
	if len(heartbeats.heartbeats) != 0 || len(regional.obs) != 0 || len(bus.events) != 0 {
		t.Fatal("failed commit produced effects")
	}
}
