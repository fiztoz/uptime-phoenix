package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Lease fencing at the service boundary (issue #63): the fence captured at
// queue time travels unchanged into the atomic commit, a rejected stale lease
// never retries (re-evaluation cannot restore authority), and a fenced result
// fails closed instead of silently recording through the legacy save.

func TestRecordPassesLeaseFenceIntoCommit(t *testing.T) {
	ctx := context.Background()
	heartbeats, regional, bus := newFakeHeartbeatRepo(), newFakeRegionalRepo(), newFakeBus()
	recorder := &fakeLocalHeartbeatRecorder{fakeRegionalRepo: regional, heartbeats: heartbeats}
	svc := NewHeartbeatService(heartbeats, bus)
	svc.SetRegionalRecorder(nil, recorder)
	fence := &domain.LeaseFence{WorkerID: "worker-a", LeaseEpoch: 7, LeaseTTL: time.Minute}

	if err := svc.Record(ctx, &domain.Monitor{ID: 1, MaxRetries: 1}, ports.CheckResult{Status: domain.StatusDown, LeaseFence: fence}); err != nil {
		t.Fatal(err)
	}
	if len(recorder.attempts) != 1 || recorder.attempts[0].LeaseFence != fence {
		t.Fatalf("fence not carried into the commit: %+v", recorder.attempts)
	}

	// A nil fence stays nil: local and push recording are preserved, not fenced.
	recorder.attempts = nil
	if err := svc.Record(ctx, &domain.Monitor{ID: 1, MaxRetries: 1}, ports.CheckResult{Status: domain.StatusUp}); err != nil {
		t.Fatal(err)
	}
	if len(recorder.attempts) != 1 || recorder.attempts[0].LeaseFence != nil {
		t.Fatalf("nil fence did not stay nil: %+v", recorder.attempts)
	}
}

func TestRecordRejectsStaleLeaseWithoutRetryOrEffects(t *testing.T) {
	ctx := context.Background()
	heartbeats, regional, bus := newFakeHeartbeatRepo(), newFakeRegionalRepo(), newFakeBus()
	recorder := &fakeLocalHeartbeatRecorder{
		fakeRegionalRepo: regional,
		heartbeats:       heartbeats,
		commitErr:        ports.ErrStaleLease,
	}
	svc := NewHeartbeatService(heartbeats, bus)
	svc.SetRegionalRecorder(nil, recorder)

	err := svc.Record(ctx, &domain.Monitor{ID: 1, MaxRetries: 2}, ports.CheckResult{
		Status:     domain.StatusDown,
		LeaseFence: &domain.LeaseFence{WorkerID: "worker-a", LeaseEpoch: 3, LeaseTTL: time.Minute},
	})
	if !errors.Is(err, ports.ErrStaleLease) {
		t.Fatalf("stale lease error = %v", err)
	}
	// ErrStaleLease must NOT fall into the ErrStaleLocalState retry loop:
	// re-evaluation cannot restore expired or replaced worker authority.
	if len(recorder.attempts) != 1 {
		t.Fatalf("stale lease was retried: %d attempts", len(recorder.attempts))
	}
	if len(heartbeats.heartbeats) != 0 || len(bus.events) != 0 {
		t.Fatalf("rejected result produced effects: hb=%d events=%d", len(heartbeats.heartbeats), len(bus.events))
	}
}

func TestRecordRetriesStaleEvaluationUnderTheSameFence(t *testing.T) {
	ctx := context.Background()
	heartbeats, regional, bus := newFakeHeartbeatRepo(), newFakeRegionalRepo(), newFakeBus()
	recorder := &fakeLocalHeartbeatRecorder{fakeRegionalRepo: regional, heartbeats: heartbeats}
	svc := NewHeartbeatService(heartbeats, bus)
	svc.SetRegionalRecorder(nil, recorder)
	fence := &domain.LeaseFence{WorkerID: "worker-a", LeaseEpoch: 5, LeaseTTL: time.Minute}
	recorder.beforeCommit = func() {
		recorder.beforeCommit = nil
		// Another check committed between evaluation and recording.
		regional.states[regionalStateKey(1, domain.LocalProbeID)] = domain.RegionalState{
			MonitorID: 1, ProbeID: domain.LocalProbeID, AssignmentGeneration: 1,
			Seq: 1, Status: domain.StatusPending, DownCount: 1,
		}
		regional.obs = append(regional.obs, domain.RegionalObservation{Seq: 1})
	}
	if err := svc.Record(ctx, &domain.Monitor{ID: 1, MaxRetries: 1}, ports.CheckResult{Status: domain.StatusDown, LeaseFence: fence}); err != nil {
		t.Fatal(err)
	}
	if len(recorder.attempts) != 2 {
		t.Fatalf("stale evaluation attempts = %d, want 2", len(recorder.attempts))
	}
	for i, attempt := range recorder.attempts {
		if attempt.LeaseFence != fence {
			t.Fatalf("attempt %d dropped the queue-time fence: %+v", i, attempt.LeaseFence)
		}
	}
}

func TestRecordFencedResultFailsClosedOnLegacyPath(t *testing.T) {
	ctx := context.Background()
	heartbeats, bus := newFakeHeartbeatRepo(), newFakeBus()
	svc := NewHeartbeatService(heartbeats, bus)
	// No regional recorder: recording would fall back to the legacy save,
	// which cannot verify worker authority atomically.

	err := svc.Record(ctx, &domain.Monitor{ID: 1}, ports.CheckResult{
		Status:     domain.StatusDown,
		LeaseFence: &domain.LeaseFence{WorkerID: "worker-a", LeaseEpoch: 3, LeaseTTL: time.Minute},
	})
	if !errors.Is(err, ports.ErrStaleLease) {
		t.Fatalf("fenced result was silently recorded through the legacy path: %v", err)
	}
	if len(heartbeats.heartbeats) != 0 || len(bus.events) != 0 {
		t.Fatalf("rejected result produced effects: hb=%d events=%d", len(heartbeats.heartbeats), len(bus.events))
	}
}
