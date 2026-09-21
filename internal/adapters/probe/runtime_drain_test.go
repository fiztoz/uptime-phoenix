package probe

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestEdgeRuntimeQuiescenceJoinsAdmittedMutation(t *testing.T) {
	r := &EdgeRuntime{}
	if !r.admitMutation() {
		t.Fatal("initial callback rejected")
	}
	done := make(chan struct{})
	go func() { r.Quiesce(); close(done) }()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		r.mu.Lock()
		draining := r.draining
		r.mu.Unlock()
		if draining {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("quiescence did not start")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-done:
		t.Fatal("already admitted mutation was not joined")
	default:
	}
	if r.admitMutation() {
		t.Fatal("new management mutation admitted")
	}
	r.mutations.Done()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("completed mutation did not release quiescence")
	}
}

func TestEdgeDrainRejectsMovingPrefixAndIdentity(t *testing.T) {
	for _, change := range []string{"producer", "stream", "cursor rollback"} {
		t.Run(change, func(t *testing.T) {
			calls := 0
			r := &EdgeRuntime{draining: true, state: func(context.Context) (domain.EdgeIdentity, int64, error) {
				calls++
				i := domain.EdgeIdentity{StreamID: "stable", LastCreatedSeq: 2, CommittedSeq: 1}
				if calls > 1 {
					switch change {
					case "producer":
						i.LastCreatedSeq++
					case "stream":
						i.StreamID = "changed"
					case "cursor rollback":
						i.CommittedSeq = 0
					}
				}
				return i, 1, nil
			}}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			result, err := r.WaitForReplay(ctx)
			if err == nil || result.Flushed || calls != 2 {
				t.Fatal("invalid drain state accepted", result, err, calls)
			}
		})
	}
}
