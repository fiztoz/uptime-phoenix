package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// CheckResult holds the outcome of a single monitor check.
type CheckResult struct {
	Status     domain.Status
	LatencyMs  int64 // primary probe latency only
	DurationMs int64 // total checker duration, including auxiliary observations
	Message    string
	Metadata   map[string]string // e.g. {"tls_days_remaining": "45"}
	Conditions []domain.ConditionObservation
}

// Checker defines the interface that every monitor type must implement.
type Checker interface {
	Type() string
	Validate(config map[string]any) error
	Check(ctx context.Context, config map[string]any) (CheckResult, error)
}
