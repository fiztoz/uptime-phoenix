package bootstrap

import (
	"context"
	"errors"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/logger"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// probeHistoryLoop resumes durable work on startup and caps each transaction
// batch. Cancellation reaches storage before the composition root closes DB.
func probeHistoryLoop(ctx context.Context, service *services.ProbeHistoryService, log *logger.SlogLogger) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		batchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		processed, err := service.ProcessBatch(batchCtx, time.Now().UTC(), 100)
		cancel()
		// Each item commits on its own, so a batch that used up its time after
		// finishing items has made progress: only the in-flight item rolled
		// back. Treating that as a failure paused a busy backlog for 2 s (#196).
		budgetSpent := errors.Is(err, context.DeadlineExceeded) && processed > 0
		if err != nil && !budgetSpent && ctx.Err() == nil {
			log.Error("probe history recomputation failed", "error", err, "processed", processed)
		}
		delay := 2 * time.Second
		if (err == nil && processed == 100) || budgetSpent {
			delay = 100 * time.Millisecond
		}
		timer.Reset(delay)
	}
}
