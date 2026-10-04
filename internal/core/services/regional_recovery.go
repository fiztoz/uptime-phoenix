package services

import (
	"context"
	"log/slog"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// regionalRecovery reads current committed policy evidence, never historical
// replay status. Missing, stale, local-only and failed reads cannot recover a page.
type regionalRecovery struct {
	overall  AggregateStatusReader
	resolver incidentAutoResolver
}

func (r regionalRecovery) resolve(ctx context.Context, ids []int64) {
	if r.overall == nil || r.resolver == nil || len(ids) == 0 {
		return
	}
	statuses, err := r.overall.StatusForMonitors(ctx, ids, time.Now().UTC())
	if err != nil {
		slog.ErrorContext(ctx, "regional recovery: overall status failed", "error", err)
		return
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		status, present := statuses[id]
		if id <= 0 || seen[id] || !present || status != domain.StatusUp {
			continue
		}
		seen[id] = true
		if err := r.resolver.AutoResolveOnRecovery(ctx, id); err != nil {
			slog.ErrorContext(ctx, "regional recovery: auto-resolve failed", "monitor_id", id, "error", err)
		}
	}
}
