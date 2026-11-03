package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/logger"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type budgetHistoryRepo struct {
	results []error
	calls   chan time.Time
}

func (r *budgetHistoryRepo) ProcessHistoryWork(_ context.Context, _ time.Time, _ int, _ ports.ProbeHistoryProjector) (int, error) {
	r.calls <- time.Now()
	if len(r.results) == 0 {
		return 0, nil
	}
	err := r.results[0]
	r.results = r.results[1:]
	return 40, err
}

// A batch that spends its time budget after committing items made progress:
// the loop must continue promptly instead of pausing a busy backlog (#196).
func TestProbeHistoryLoopContinuesAfterSpentBudget(t *testing.T) {
	repo := &budgetHistoryRepo{results: []error{context.DeadlineExceeded}, calls: make(chan time.Time, 4)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		probeHistoryLoop(ctx, services.NewProbeHistoryService(repo), logger.New("error"))
		close(done)
	}()
	first := <-repo.calls
	var second time.Time
	select {
	case second = <-repo.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not resume")
	}
	cancel()
	<-done
	if gap := second.Sub(first); gap >= time.Second {
		t.Fatalf("loop paused %s after a batch that made progress", gap)
	}
}

// A real failure still backs off.
func TestProbeHistoryLoopBacksOffAfterFailure(t *testing.T) {
	repo := &budgetHistoryRepo{results: []error{errors.New("boom")}, calls: make(chan time.Time, 4)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		probeHistoryLoop(ctx, services.NewProbeHistoryService(repo), logger.New("error"))
		close(done)
	}()
	first := <-repo.calls
	var second time.Time
	select {
	case second = <-repo.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not resume")
	}
	cancel()
	<-done
	if gap := second.Sub(first); gap < time.Second {
		t.Fatalf("loop retried a failed batch after %s", gap)
	}
}
