package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestLocalHeartbeatContentionWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		heartbeats, regional, bus := newFakeHeartbeatRepo(), newFakeRegionalRepo(), newFakeBus()
		recorder := &fakeLocalHeartbeatRecorder{fakeRegionalRepo: regional, heartbeats: heartbeats, commitErr: ports.ErrStaleLocalState}
		svc := NewHeartbeatService(heartbeats, bus)
		svc.SetRegionalRecorder(nil, recorder)
		start := time.Now()
		// A short-lived competing transaction must get time to finish before all
		// sixteen complete attempts are consumed. Fake time makes this deterministic.
		recorder.beforeCommit = func() {
			if time.Since(start) >= 20*time.Millisecond {
				recorder.commitErr = nil
			}
		}
		fence := &domain.LeaseFence{WorkerID: "worker-a", LeaseEpoch: 7, LeaseTTL: time.Minute}
		err := svc.Record(t.Context(), &domain.Monitor{ID: 1, MaxRetries: 1}, ports.CheckResult{Status: domain.StatusDown, ConfigRevision: 3, AssignmentGeneration: 1, LeaseFence: fence})
		if err != nil {
			t.Fatalf("short contention window dropped check: %v (%d attempts)", err, len(recorder.attempts))
		}
		if len(recorder.attempts) < 2 || len(recorder.attempts) > 16 {
			t.Fatalf("attempts=%d", len(recorder.attempts))
		}
		first := recorder.attempts[0]
		for _, a := range recorder.attempts {
			if !a.Heartbeat.Time.Equal(first.Heartbeat.Time) || !a.Heartbeat.ReceivedAt.Equal(first.Heartbeat.ReceivedAt) || a.RawStatus != first.RawStatus || a.Heartbeat.ConfigRevision != 3 || a.Heartbeat.AssignmentGeneration != 1 || a.LeaseFence != fence {
				t.Fatal("retry changed captured result or authority")
			}
		}
		if len(heartbeats.heartbeats) != 1 || len(regional.obs) != 1 || len(bus.events) != 2 {
			t.Fatalf("effects: heartbeats=%d observations=%d events=%d", len(heartbeats.heartbeats), len(regional.obs), len(bus.events))
		}
	})
}

func TestLocalHeartbeatContentionCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, r, b := newFakeHeartbeatRepo(), newFakeRegionalRepo(), newFakeBus()
		rec := &fakeLocalHeartbeatRecorder{fakeRegionalRepo: r, heartbeats: h, commitErr: ports.ErrStaleLocalState}
		svc := NewHeartbeatService(h, b)
		svc.SetRegionalRecorder(nil, rec)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Millisecond)
		defer cancel()
		err := svc.Record(ctx, &domain.Monitor{ID: 1}, ports.CheckResult{Status: domain.StatusUp})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancellation=%v", err)
		}
		if len(rec.attempts) != 1 || len(h.heartbeats) != 0 || len(b.events) != 0 {
			t.Fatal("canceled wait retried or published")
		}
	})
}

func TestLocalHeartbeatContentionBoundAndRedaction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, r, b := newFakeHeartbeatRepo(), newFakeRegionalRepo(), newFakeBus()
		rec := &fakeLocalHeartbeatRecorder{fakeRegionalRepo: r, heartbeats: h, commitErr: fmt.Errorf("private SQL payload: %w", ports.ErrStaleLocalState)}
		svc := NewHeartbeatService(h, b)
		svc.SetRegionalRecorder(nil, rec)
		start := time.Now()
		err := svc.Record(t.Context(), &domain.Monitor{ID: 1}, ports.CheckResult{Status: domain.StatusUp})
		if !errors.Is(err, ports.ErrStaleLocalState) || strings.Contains(err.Error(), "private SQL") {
			t.Fatalf("unsafe/incorrect exhaustion error: %v", err)
		}
		if len(rec.attempts) != 16 || len(h.heartbeats) != 0 || len(b.events) != 0 {
			t.Fatal("attempt bound/effects changed")
		}
		if elapsed := time.Since(start); elapsed < 515*time.Millisecond || elapsed > 575*time.Millisecond {
			t.Fatalf("retry pacing budget=%s", elapsed)
		}
	})
}

func TestLocalHeartbeatContentionAuthorityRejectionStaysTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"lease", ports.ErrStaleLease}, {"configuration", ports.ErrConflict}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, r, b := newFakeHeartbeatRepo(), newFakeRegionalRepo(), newFakeBus()
				rec := &fakeLocalHeartbeatRecorder{fakeRegionalRepo: r, heartbeats: h, commitErr: ports.ErrStaleLocalState}
				svc := NewHeartbeatService(h, b)
				svc.SetRegionalRecorder(nil, rec)
				rec.beforeCommit = func() {
					if len(rec.attempts) == 2 {
						rec.commitErr = tc.err
					}
				}
				fence := &domain.LeaseFence{WorkerID: "worker-a", LeaseEpoch: 7, LeaseTTL: time.Minute}
				err := svc.Record(t.Context(), &domain.Monitor{ID: 1}, ports.CheckResult{Status: domain.StatusUp, ConfigRevision: 3, AssignmentGeneration: 1, LeaseFence: fence})
				if !errors.Is(err, tc.err) || len(rec.attempts) != 2 || len(h.heartbeats) != 0 || len(b.events) != 0 {
					t.Fatalf("terminal authority rejection retried or published: %v (%d attempts)", err, len(rec.attempts))
				}
				for _, a := range rec.attempts {
					if a.Heartbeat.ConfigRevision != 3 || a.Heartbeat.AssignmentGeneration != 1 || a.LeaseFence != fence {
						t.Fatal("authority retargeted")
					}
				}
			})
		})
	}
}
