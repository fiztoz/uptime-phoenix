package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// NotificationDispatcher decides whether a recorded heartbeat warrants an alert
// and dispatches it to the monitor's notification providers. Implementations
// apply maintenance-window suppression and resend throttling.
//
// It is invoked from the heartbeat service in the monitor's owning worker (the
// single local worker, or the worker holding the lease in sharded mode) so each
// status transition triggers at most one alert — unlike subscribing to the
// EventBus, which fans out to every worker under a Redis-backed bus.
type NotificationDispatcher interface {
	// OnHeartbeat is called for every recorded heartbeat. prevStatus is the
	// monitor's previous effective status, or nil if this is the first heartbeat.
	OnHeartbeat(ctx context.Context, monitor *domain.Monitor, hb *domain.Heartbeat, prevStatus *domain.Status)
}
