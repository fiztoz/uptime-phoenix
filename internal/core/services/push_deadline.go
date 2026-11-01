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

// PushDeadlineGrace is added to a push monitor's interval before a push
// counts as overdue: heartbeat times are stored with second precision
// (truncated on MariaDB), and the scheduler's check and the pusher's schedule
// both jitter by up to about a second. Without it a pusher that is exactly on
// time against an equal interval flaps DOWN/UP (#161 review).
const PushDeadlineGrace = 2 * time.Second

// EvaluatePushDeadline decides what the scheduler records for a passive push
// monitor at now. Push monitors follow Uptime Kuma semantics: the push
// endpoint records UP/DOWN as pushes arrive, and when no push has arrived
// within the monitor's interval (plus PushDeadlineGrace) the monitor is
// observed DOWN (retries then apply as for any failing check). record is
// false while the latest push is still fresh, so the scheduler adds nothing
// between pushes. A monitor that has never been pushed gets one full interval
// from its creation first.
//
// armedAt is when the calling scheduler started evaluating this monitor (its
// process start, the monitor's resume, or a new assignment or lease); zero
// means unknown. The deadline counts from the later of the latest push and
// armedAt, so a restart or a pause never becomes a DOWN on its own: the
// pusher always gets one interval after the scheduler is watching again.
func (s *HeartbeatService) EvaluatePushDeadline(ctx context.Context, monitor *domain.Monitor, now, armedAt time.Time) (result ports.CheckResult, record bool, err error) {
	if monitor == nil || monitor.Interval <= 0 {
		return ports.CheckResult{}, false, nil
	}
	now = now.UTC()
	deadline := time.Duration(monitor.Interval)*time.Second + PushDeadlineGrace
	missed := ports.CheckResult{
		Status:  domain.StatusDown,
		Message: fmt.Sprintf("%s in the last %ds", PushMissedMessagePrefix, monitor.Interval),
	}
	// overdue reports whether more than the deadline has passed since ref and
	// since the scheduler armed the monitor. A zero ref is "long ago".
	overdue := func(ref time.Time) bool {
		if !armedAt.IsZero() && armedAt.After(ref) {
			ref = armedAt
		}
		return ref.IsZero() || now.Sub(ref.UTC()) > deadline
	}

	latest, err := s.heartbeats.GetLatest(ctx, monitor.ID)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return ports.CheckResult{}, false, fmt.Errorf("push deadline: latest heartbeat: %w", err)
	}
	var ref time.Time
	switch {
	case err != nil || latest == nil:
		ref = monitor.CreatedAt
	case strings.HasPrefix(latest.Msg, PushMissedMessagePrefix):
		// Our own overdue observation is the latest row: still no push since,
		// so only the arming grace can hold the next observation back.
		ref = time.Time{}
	default:
		ref = latest.Time
	}
	if !overdue(ref) {
		return ports.CheckResult{}, false, nil
	}
	return missed, true, nil
}
