package bootstrap

import (
	"context"
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
		if err != nil && ctx.Err() == nil {
			log.Error("probe history recomputation failed", "error", err)
		}
		delay := 2 * time.Second
		if err == nil && processed == 100 {
			delay = 100 * time.Millisecond
		}
		timer.Reset(delay)
	}
}
