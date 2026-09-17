package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// NotificationThrottleRepository persists availability notification attempt times.
// Reservation precedes provider I/O, preserving the dispatcher's existing
// attempt-based backoff. This is not a delivery queue or an execution lease.
type NotificationThrottleRepository interface {
	// Reserve atomically checks and records an eligible attempt. A zero interval
	// permits an immediate transition; positive intervals throttle resends.
	// Missing state is immediately eligible. Clock rollback never lowers a cursor.
	Reserve(ctx context.Context, key domain.NotificationThrottleKey, at time.Time, interval time.Duration) (bool, error)
	// Clear removes only this assignment's cursor after recovery.
	Clear(ctx context.Context, key domain.NotificationThrottleKey) error
}
