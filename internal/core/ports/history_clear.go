package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// HistoryClearStore implements the authorized clear-history action.
//
// ClearMonitorHistory removes one monitor's history evidence — raw heartbeats,
// regional observations, rollups and materialized history windows — and
// upserts one clear-history watermark per active remote assignment. The
// watermark is what keeps the deletion durable: later replay of cleared
// evidence is dropped and acknowledged instead of resurrected. Current state,
// incidents, delivery outcomes and assignment rows are not history and are
// untouched. The returned watermarks are the installed fences; `at` is the
// operator's clear time and must be UTC-normalized before storage.
//
// One call is one deliberate deletion. Repeating it widens the fence (the
// bounds never shrink) and keeps counting intentional drops.
type HistoryClearStore interface {
	ClearMonitorHistory(ctx context.Context, monitorID int64, at time.Time) ([]domain.HistoryClearWatermark, error)
}
