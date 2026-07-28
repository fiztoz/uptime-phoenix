package services

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// alertNotifier dispatches an alert to a monitor's assigned providers.
// Satisfied by *NotificationService.
type alertNotifier interface {
	Notify(ctx context.Context, monitor *domain.Monitor, status, prevStatus domain.Status) error
}

// ackURLNotifier optionally attaches a deep-link acknowledgement URL and the
// heartbeat check message (CheckOutput) to a notification. Satisfied by
// *NotificationService; plain fakes that only implement Notify keep working.
type ackURLNotifier interface {
	NotifyWithAck(ctx context.Context, monitor *domain.Monitor, status, prevStatus domain.Status, ackURL, checkOutput string) error
}

// maintenanceChecker reports whether a monitor is currently inside an active
// maintenance window. Satisfied by *MaintenanceService.
type maintenanceChecker interface {
	IsActive(ctx context.Context, monitorID int64) (bool, error)
}

// incidentAutoResolver resolves status-page incidents when a monitor recovers.
// Satisfied by *StatusPageService. Optional — when nil, recovery only alerts.
type incidentAutoResolver interface {
	AutoResolveOnRecovery(ctx context.Context, monitorID int64) error
}

// groupEvaluator re-evaluates the folders containing a monitor and alerts on any
// whose own derived status transitioned. Satisfied by *GroupAlertService.
// Optional — when nil, folders simply never alert.
type groupEvaluator interface {
	OnHeartbeat(ctx context.Context, monitor *domain.Monitor)
}

// alertLifecycle persists the monitor alert entity (F2.2): open on DOWN, resolve
// on recovery, answer "is resend suppressed by ack?". Optional — when nil the
// dispatcher behaves as before (no lifecycle rows, no ack suppression).
// Satisfied by *AlertService.
type alertLifecycle interface {
	OpenOnDown(ctx context.Context, monitor *domain.Monitor, at time.Time) (*domain.Alert, error)
	ResolveOpen(ctx context.Context, monitorID int64, at time.Time) error
	IsOpenAcked(ctx context.Context, monitorID int64) (bool, error)
}

// escalationStarter begins an alert's escalation ladder (F2.3). Satisfied by
// *EscalationService. Optional — when nil, nothing escalates.
//
// It is deliberately a SEPARATE interface rather than three more methods on
// alertLifecycle: widening a port breaks every hand-written fake, including the
// ones in hub_rbac_test.go that must stay green unmodified to prove fail-closed
// RBAC survived (handoff §4.5).
type escalationStarter interface {
	StartForAlert(ctx context.Context, alert *domain.Alert, monitor *domain.Monitor) error
}

// NotificationDispatcher implements ports.NotificationDispatcher. It turns
// heartbeat status transitions into alerts, applying:
//   - maintenance suppression (no alerts during a maintenance window),
//   - confirmed-transition gating (alert on DOWN and on recovery, never on the
//     intermediate PENDING state produced by the retry window),
//   - resend throttling (re-alert a still-DOWN monitor at most once per
//     ResendInterval),
//   - acknowledgement suppression (F2.2: no resend while the open alert is acked),
//   - optional auto-resolve of status-page incidents on recovery.
//
// last-notified timestamps are held in memory: on a worker restart or lease
// hand-off the throttle resets, costing at most one extra resend. A persisted
// column is a future refinement. The alert entity itself IS persisted.
type NotificationDispatcher struct {
	notifier    alertNotifier
	maintenance maintenanceChecker
	autoResolve incidentAutoResolver // optional
	groups      groupEvaluator       // optional — folder alerting
	lifecycle   alertLifecycle       // optional — F2.2 alert entity
	escalation  escalationStarter    // optional — F2.3 escalation ladder
	publicURL   string               // optional — for deep-link AckURL

	mu           sync.Mutex
	lastNotified map[int64]time.Time
	now          func() time.Time // injectable clock for tests
}

// NewNotificationDispatcher creates a dispatcher backed by the notification and
// maintenance services.
func NewNotificationDispatcher(notifier alertNotifier, maintenance maintenanceChecker) *NotificationDispatcher {
	return &NotificationDispatcher{
		notifier:     notifier,
		maintenance:  maintenance,
		lastNotified: make(map[int64]time.Time),
		now:          time.Now,
	}
}

// SetAutoResolver wires incident auto-resolve on monitor recovery. Optional.
func (d *NotificationDispatcher) SetAutoResolver(r incidentAutoResolver) {
	d.autoResolve = r
}

// SetGroupEvaluator wires folder (monitor group) alerting. Optional — without it
// notifications attached to a folder never fire.
func (d *NotificationDispatcher) SetGroupEvaluator(e groupEvaluator) {
	d.groups = e
}

// SetAlertLifecycle wires F2.2 alert entity open/ack/resolve. Optional.
func (d *NotificationDispatcher) SetAlertLifecycle(l alertLifecycle) {
	d.lifecycle = l
}

// SetEscalationStarter wires F2.3 escalation ladders. Optional — without it a
// confirmed DOWN still notifies exactly as before, it just never escalates.
func (d *NotificationDispatcher) SetEscalationStarter(e escalationStarter) {
	d.escalation = e
}

// SetPublicURL sets the absolute public origin used to build deep-link ack URLs
// (e.g. https://status.example.com). Empty disables AckURL injection.
func (d *NotificationDispatcher) SetPublicURL(url string) {
	d.publicURL = strings.TrimRight(strings.TrimSpace(url), "/")
}

// OnHeartbeat evaluates a recorded heartbeat and dispatches an alert when the
// effective status transition warrants one.
func (d *NotificationDispatcher) OnHeartbeat(ctx context.Context, monitor *domain.Monitor, hb *domain.Heartbeat, prevStatus *domain.Status) {
	// Folder alerting runs on EVERY heartbeat, and deliberately BEFORE this
	// monitor's maintenance suppression below. A monitor inside a maintenance
	// window still records a MAINTENANCE heartbeat, and that changes the rollup of
	// the folders above it (Rollup excludes maintenance children from the tally).
	// Returning early would leave those folders' persisted status stale — and a
	// folder has its own maintenance semantics anyway: it never alerts on
	// MAINTENANCE, whatever its monitors are doing.
	if d.groups != nil {
		d.groups.OnHeartbeat(ctx, monitor)
	}

	// Never alert while a maintenance window is active for this monitor.
	if active, err := d.maintenance.IsActive(ctx, monitor.ID); err != nil {
		slog.Warn("notification dispatcher: maintenance check failed, alerting anyway",
			"monitor_id", monitor.ID, "error", err)
	} else if active {
		return
	}

	if hb == nil {
		return
	}

	cur := hb.Status
	prev := domain.StatusUp // first-ever heartbeat is treated as a transition from UP
	if prevStatus != nil {
		prev = *prevStatus
	}
	now := d.now()

	checkOutput := hb.Msg

	switch {
	case cur == domain.StatusDown && prev != domain.StatusDown:
		// Confirmed failure (the retry window, if any, is already exhausted).
		alert, ackURL := d.openAlert(ctx, monitor, now)
		// STEP ZERO. This send belongs to the dispatcher and to nothing else.
		// The escalation policy owns steps 1..N and starts only after this
		// line, so the initial notification can be neither lost nor duplicated
		// by a policy (docs/F2.3-ESCALATION-CONTRACTS.md, contract 2).
		d.dispatch(ctx, monitor, cur, prev, now, ackURL, checkOutput)
		d.startEscalation(ctx, monitor, alert)
	case cur == domain.StatusUp && prev == domain.StatusDown:
		// Recovery — resolve the open alert entity, notify, clear resend throttle,
		// auto-resolve status-page incidents.
		d.resolveAlert(ctx, monitor.ID, now)
		d.dispatch(ctx, monitor, cur, prev, now, "", checkOutput)
		d.forget(monitor.ID)
		if d.autoResolve != nil {
			if err := d.autoResolve.AutoResolveOnRecovery(ctx, monitor.ID); err != nil {
				slog.Error("notification dispatcher: auto-resolve failed",
					"monitor_id", monitor.ID, "error", err)
			}
		}
	case cur == domain.StatusDown && prev == domain.StatusDown:
		// Still down — re-alert only once per ResendInterval (minutes), and never
		// while the open alert is acknowledged (F2.2).
		if d.lifecycle != nil {
			acked, err := d.lifecycle.IsOpenAcked(ctx, monitor.ID)
			if err != nil {
				slog.Warn("notification dispatcher: ack check failed, continuing resend logic",
					"monitor_id", monitor.ID, "error", err)
			} else if acked {
				return
			}
		}
		if monitor.ResendInterval > 0 && d.dueForResend(monitor.ID, monitor.ResendInterval, now) {
			ackURL := d.ackURLForOpen(ctx, monitor)
			d.dispatch(ctx, monitor, cur, prev, now, ackURL, checkOutput)
		}
	}
	// PENDING transitions (UP→PENDING, PENDING→UP) intentionally do not alert:
	// that is the whole point of the retry window.
}

// openAlert opens (or re-reads) the monitor's alert entity and returns it along
// with its deep-link ack URL. Both may be zero when F2.2 is not wired or the
// open failed — the notification still goes out either way.
func (d *NotificationDispatcher) openAlert(ctx context.Context, monitor *domain.Monitor, now time.Time) (*domain.Alert, string) {
	if d.lifecycle == nil {
		return nil, ""
	}
	a, err := d.lifecycle.OpenOnDown(ctx, monitor, now)
	if err != nil {
		slog.Error("notification dispatcher: open alert failed",
			"monitor_id", monitor.ID, "error", err)
		return nil, ""
	}
	return a, d.buildAckURL(a)
}

// startEscalation begins the F2.3 ladder for a just-opened alert. A failure here
// must not fail the heartbeat path: the operator has already been notified by
// step zero, and an escalation that never started is strictly better than a
// dropped check cycle.
func (d *NotificationDispatcher) startEscalation(ctx context.Context, monitor *domain.Monitor, alert *domain.Alert) {
	if d.escalation == nil || alert == nil {
		return
	}
	if err := d.escalation.StartForAlert(ctx, alert, monitor); err != nil {
		slog.Error("notification dispatcher: start escalation failed",
			"monitor_id", monitor.ID, "alert_id", alert.ID, "error", err)
	}
}

func (d *NotificationDispatcher) resolveAlert(ctx context.Context, monitorID int64, now time.Time) {
	if d.lifecycle == nil {
		return
	}
	if err := d.lifecycle.ResolveOpen(ctx, monitorID, now); err != nil {
		slog.Error("notification dispatcher: resolve alert failed",
			"monitor_id", monitorID, "error", err)
	}
}

func (d *NotificationDispatcher) ackURLForOpen(ctx context.Context, monitor *domain.Monitor) string {
	if d.lifecycle == nil || d.publicURL == "" {
		return ""
	}
	// OpenOnDown is idempotent for an already-open alert and returns its token.
	a, err := d.lifecycle.OpenOnDown(ctx, monitor, d.now())
	if err != nil {
		return ""
	}
	return d.buildAckURL(a)
}

func (d *NotificationDispatcher) buildAckURL(a *domain.Alert) string {
	if a == nil || a.AckToken == "" || d.publicURL == "" {
		return ""
	}
	return d.publicURL + "/ack/" + a.AckToken
}

func (d *NotificationDispatcher) dispatch(ctx context.Context, monitor *domain.Monitor, status, prev domain.Status, now time.Time, ackURL, checkOutput string) {
	d.mu.Lock()
	d.lastNotified[monitor.ID] = now
	d.mu.Unlock()

	// Prefer NotifyWithAck when available so checkOutput (and optional ackURL)
	// always reach AlertContext — even when ackURL is empty on recovery.
	var err error
	if an, ok := d.notifier.(ackURLNotifier); ok {
		err = an.NotifyWithAck(ctx, monitor, status, prev, ackURL, checkOutput)
	} else {
		err = d.notifier.Notify(ctx, monitor, status, prev)
	}
	if err != nil {
		slog.Error("notification dispatcher: notify failed",
			"monitor_id", monitor.ID, "status", status.String(), "error", err)
	}
}

func (d *NotificationDispatcher) dueForResend(monitorID int64, resendMinutes int, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	last, ok := d.lastNotified[monitorID]
	if !ok {
		// Down but never notified (e.g. the original transition was suppressed by
		// a maintenance window that has since ended) — alert now.
		return true
	}
	return now.Sub(last) >= time.Duration(resendMinutes)*time.Minute
}

func (d *NotificationDispatcher) forget(monitorID int64) {
	d.mu.Lock()
	delete(d.lastNotified, monitorID)
	d.mu.Unlock()
}
