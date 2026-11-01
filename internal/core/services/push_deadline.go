package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// PushMissedMessagePrefix starts the message of every heartbeat the scheduler
// records for a push monitor whose push is overdue. It is how the deadline
// tells its own observations apart from real pushes.
const PushMissedMessagePrefix = "push: no push received"

// EvaluatePushDeadline decides what the scheduler records for a passive push
// monitor at now. Push monitors follow Uptime Kuma semantics: the push
// endpoint records UP/DOWN as pushes arrive, and when no push has arrived
// within the monitor's interval the monitor is observed DOWN (retries then
// apply as for any failing check). record is false while the latest push is
// still fresh, so the scheduler adds nothing between pushes. A monitor that
// has never been pushed gets one full interval from its creation first.
func (s *HeartbeatService) EvaluatePushDeadline(ctx context.Context, monitor *domain.Monitor, now time.Time) (result ports.CheckResult, record bool, err error) {
	if monitor == nil || monitor.Interval <= 0 {
		return ports.CheckResult{}, false, nil
	}
	now = now.UTC()
	interval := time.Duration(monitor.Interval) * time.Second
	missed := ports.CheckResult{
		Status:  domain.StatusDown,
		Message: fmt.Sprintf("%s in the last %ds", PushMissedMessagePrefix, monitor.Interval),
	}

	latest, err := s.heartbeats.GetLatest(ctx, monitor.ID)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return ports.CheckResult{}, false, fmt.Errorf("push deadline: latest heartbeat: %w", err)
	}
	if err != nil || latest == nil {
		if !monitor.CreatedAt.IsZero() && now.Sub(monitor.CreatedAt.UTC()) < interval {
			return ports.CheckResult{}, false, nil
		}
		return missed, true, nil
	}
	if strings.HasPrefix(latest.Msg, PushMissedMessagePrefix) {
		// Our own overdue observation is the latest row: still no push since.
		return missed, true, nil
	}
	if now.Sub(latest.Time.UTC()) < interval {
		return ports.CheckResult{}, false, nil
	}
	return missed, true, nil
}
