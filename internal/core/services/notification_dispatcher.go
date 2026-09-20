package services

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
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

// alertDetailsNotifier carries lifecycle timing in addition to the legacy
// acknowledgement/check-output fields. NotificationService implements it;
// small test fakes may keep implementing only alertNotifier or ackURLNotifier.
type alertDetailsNotifier interface {
	NotifyWithAlertDetails(
		ctx context.Context,
		monitor *domain.Monitor,
		status, prevStatus domain.Status,
		ackURL, checkOutput string,
		startedAt time.Time,
		duration time.Duration,
	) error
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

// alertLifecycleResolver is the richer recovery path implemented by
// AlertService. Keeping it separate preserves the small alertLifecycle test
// contract while allowing production recovery emails to include FiredAt.
type alertLifecycleResolver interface {
	ResolveOpenWithAlert(ctx context.Context, monitorID int64, at time.Time) (*domain.Alert, error)
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
// Production stores attempt timestamps per assignment in the database. A
// process-local fallback supports isolated callers without a persistence port.
// Provider failures still consume the resend interval; durable delivery retries
// belong to the later incident/outbox path. This dispatcher handles local only.
type NotificationDispatcher struct {
	notifier    alertNotifier
	maintenance maintenanceChecker
	autoResolve incidentAutoResolver // optional
	groups      groupEvaluator       // optional — folder alerting
	lifecycle   alertLifecycle       // optional — F2.2 alert entity
	escalation  escalationStarter    // optional — F2.3 escalation ladder
	publicURL   string               // optional — for deep-link AckURL
	throttles   ports.NotificationThrottleRepository
	assignments ports.MonitorProbeAssignmentRepository

	outboxDelivery bool // when true, availability alerts are dispatched by outbox consumer

	mu           sync.Mutex
	lastNotified map[domain.NotificationThrottleKey]time.Time
	now          func() time.Time // injectable clock for tests
}

// NewNotificationDispatcher creates a dispatcher backed by the notification and
// maintenance services.
func NewNotificationDispatcher(notifier alertNotifier, maintenance maintenanceChecker) *NotificationDispatcher {
	return &NotificationDispatcher{
		notifier:     notifier,
		maintenance:  maintenance,
		lastNotified: make(map[domain.NotificationThrottleKey]time.Time),
		now:          time.Now,
	}
}

// SetThrottleRepository attaches durable, assignment-scoped resend throttles.
// Configure at startup before the dispatcher receives heartbeats.
func (d *NotificationDispatcher) SetThrottleRepository(repo ports.NotificationThrottleRepository) {
	d.throttles = repo
}

// SetAssignmentRepository fences obsolete local generations before any side effect.
func (d *NotificationDispatcher) SetAssignmentRepository(repo ports.MonitorProbeAssignmentRepository) {
	d.assignments = repo
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

// SetOutboxDelivery enables or disables outbox delivery mode. When enabled,
// availability alert dispatching is owned by the delivery outbox consumer and
// skipped here to prevent duplicate sends, while folder alerting, escalation,
// and auto-resolve remain active.
func (d *NotificationDispatcher) SetOutboxDelivery(enabled bool) {
	d.outboxDelivery = enabled
}

// OnHeartbeat evaluates a recorded heartbeat and dispatches an alert when the
// effective status transition warrants one.
func (d *NotificationDispatcher) OnHeartbeat(ctx context.Context, monitor *domain.Monitor, hb *domain.Heartbeat, prevStatus *domain.Status) {
	// Remote replay must never enter legacy lifecycle, group, or provider work.
	// Omitted identity/generation belongs to the legacy local generation one.
	if monitor == nil || hb == nil || hb.MonitorID != monitor.ID || monitor.ID <= 0 ||
		domain.NormalizeProbeID(hb.ProbeID) != domain.LocalProbeID || hb.AssignmentGeneration < 0 {
		return
	}
	key := domain.NotificationThrottleKey{MonitorID: monitor.ID, ProbeID: domain.LocalProbeID, AssignmentGeneration: hb.AssignmentGeneration}
	if key.AssignmentGeneration == 0 {
		key.AssignmentGeneration = 1
	}
	active, err := localAlertAssignmentActive(ctx, d.assignments, key.MonitorID, key.ProbeID, key.AssignmentGeneration)
	if err != nil {
		slog.Error("notification dispatcher: assignment lookup failed", "monitor_id", monitor.ID, "error", err)
		return
	}
	if !active {
		return
	}
	lifecycle := d.lifecycle
	if scoped, ok := lifecycle.(interface {
		ForAssignment(string, int64) (*AlertService, error)
	}); ok {
		lifecycle, err = scoped.ForAssignment(key.ProbeID, key.AssignmentGeneration)
		if err != nil {
			slog.Error("notification dispatcher: bind lifecycle failed", "monitor_id", monitor.ID, "error", err)
			return
		}
	} else if lifecycle != nil && key.AssignmentGeneration != 1 {
		// Legacy test/custom lifecycles cannot safely address a newer generation.
		return
	}
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

	cur := hb.Status
	prev := domain.StatusUp // first-ever heartbeat is treated as a transition from UP
	if prevStatus != nil {
		prev = *prevStatus
	}
	now := d.now().UTC()

	checkOutput := hb.Msg

	switch {
	case cur == domain.StatusDown && prev != domain.StatusDown:
		// Confirmed failure (the retry window, if any, is already exhausted).
		if !d.outboxDelivery && !d.reserveAttempt(ctx, key, now, 0) {
			return
		}
		alert, ackURL := d.openAlert(ctx, lifecycle, monitor, now)
		// STEP ZERO. This send belongs to the dispatcher and to nothing else.
		// The escalation policy owns steps 1..N and starts only after this
		// line, so the initial notification can be neither lost nor duplicated
		// by a policy (docs/F2.3-ESCALATION-CONTRACTS.md, contract 2).
		if !d.outboxDelivery {
			startedAt, duration := alertLifecycleTiming(alert, now)
			d.dispatch(ctx, monitor, cur, prev, ackURL, checkOutput, startedAt, duration)
		}
		d.startEscalation(ctx, monitor, alert)
	case cur == domain.StatusUp && prev == domain.StatusDown:
		// Recovery — resolve the open alert entity, notify, clear resend throttle,
		// auto-resolve status-page incidents.
		alert := d.resolveAlert(ctx, lifecycle, monitor.ID, now)
		if !d.outboxDelivery {
			startedAt, duration := alertLifecycleTiming(alert, now)
			d.dispatch(ctx, monitor, cur, prev, "", checkOutput, startedAt, duration)
		}
		d.forget(ctx, key)
		if d.autoResolve != nil {
			if err := d.autoResolve.AutoResolveOnRecovery(ctx, monitor.ID); err != nil {
				slog.Error("notification dispatcher: auto-resolve failed",
					"monitor_id", monitor.ID, "error", err)
			}
		}
	case cur == domain.StatusDown && prev == domain.StatusDown:
		if d.outboxDelivery {
			return
		}
		// Still down — re-alert only once per ResendInterval (minutes), and never
		// while the open alert is acknowledged (F2.2).
		if lifecycle != nil {
			acked, err := lifecycle.IsOpenAcked(ctx, monitor.ID)
			if err != nil {
				slog.Warn("notification dispatcher: ack check failed, continuing resend logic",
					"monitor_id", monitor.ID, "error", err)
			} else if acked {
				return
			}
		}
		if monitor.ResendInterval > 0 && d.reserveAttempt(ctx, key, now, time.Duration(monitor.ResendInterval)*time.Minute) {
			alert, ackURL := d.openAlertForResend(ctx, lifecycle, monitor, now)
			if !d.outboxDelivery {
				startedAt, duration := alertLifecycleTiming(alert, now)
				d.dispatch(ctx, monitor, cur, prev, ackURL, checkOutput, startedAt, duration)
			}
		}
	}
	// PENDING transitions (UP→PENDING, PENDING→UP) intentionally do not alert:
	// that is the whole point of the retry window.
}

// openAlert opens (or re-reads) the monitor's alert entity and returns it along
// with its deep-link ack URL. Both may be zero when F2.2 is not wired or the
// open failed — the notification still goes out either way.
func (d *NotificationDispatcher) openAlert(ctx context.Context, lifecycle alertLifecycle, monitor *domain.Monitor, now time.Time) (*domain.Alert, string) {
	if lifecycle == nil {
		return nil, ""
	}
	a, err := lifecycle.OpenOnDown(ctx, monitor, now)
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

func (d *NotificationDispatcher) resolveAlert(ctx context.Context, lifecycle alertLifecycle, monitorID int64, now time.Time) *domain.Alert {
	if lifecycle == nil {
		return nil
	}
	if resolver, ok := lifecycle.(alertLifecycleResolver); ok {
		alert, err := resolver.ResolveOpenWithAlert(ctx, monitorID, now)
		if err != nil {
			slog.Error("notification dispatcher: resolve alert failed",
				"monitor_id", monitorID, "error", err)
			return nil
		}
		return alert
	}
	if err := lifecycle.ResolveOpen(ctx, monitorID, now); err != nil {
		slog.Error("notification dispatcher: resolve alert failed",
			"monitor_id", monitorID, "error", err)
	}
	return nil
}

func (d *NotificationDispatcher) openAlertForResend(ctx context.Context, lifecycle alertLifecycle, monitor *domain.Monitor, now time.Time) (*domain.Alert, string) {
	if lifecycle == nil {
		return nil, ""
	}
	// OpenOnDown is idempotent for an already-open alert and returns its token.
	a, err := lifecycle.OpenOnDown(ctx, monitor, now)
	if err != nil {
		return nil, ""
	}
	return a, d.buildAckURL(a)
}

func (d *NotificationDispatcher) buildAckURL(a *domain.Alert) string {
	if a == nil || a.AckToken == "" || d.publicURL == "" {
		return ""
	}
	return d.publicURL + "/ack/" + a.AckToken
}

func (d *NotificationDispatcher) dispatch(
	ctx context.Context,
	monitor *domain.Monitor,
	status, prev domain.Status,
	ackURL, checkOutput string,
	startedAt time.Time,
	duration time.Duration,
) {
	// Prefer the richest supported contract so lifecycle timing, checkOutput,
	// and the optional ackURL all reach AlertContext on the same delivery.
	var err error
	if detailed, ok := d.notifier.(alertDetailsNotifier); ok {
		err = detailed.NotifyWithAlertDetails(
			ctx, monitor, status, prev, ackURL, checkOutput, startedAt, duration,
		)
	} else if an, ok := d.notifier.(ackURLNotifier); ok {
		err = an.NotifyWithAck(ctx, monitor, status, prev, ackURL, checkOutput)
	} else {
		err = d.notifier.Notify(ctx, monitor, status, prev)
	}
	if err != nil {
		slog.Error("notification dispatcher: notify failed",
			"monitor_id", monitor.ID, "status", status.String(), "error", err)
	}
}

func alertLifecycleTiming(alert *domain.Alert, now time.Time) (time.Time, time.Duration) {
	if alert == nil || alert.FiredAt.IsZero() {
		return time.Time{}, 0
	}
	startedAt := alert.FiredAt.UTC()
	duration := now.UTC().Sub(startedAt)
	if duration < 0 {
		duration = 0
	}
	return startedAt, duration
}

func (d *NotificationDispatcher) reserveAttempt(ctx context.Context, key domain.NotificationThrottleKey, now time.Time, interval time.Duration) bool {
	if d.throttles != nil {
		reserved, err := d.throttles.Reserve(ctx, key, now.UTC(), interval)
		if err != nil {
			slog.Error("notification dispatcher: reserve attempt failed", "monitor_id", key.MonitorID, "error", err)
			return false
		}
		return reserved
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	last, ok := d.lastNotified[key]
	if ok && interval > 0 && now.Sub(last) < interval {
		return false
	}
	if !ok || now.After(last) {
		d.lastNotified[key] = now
	}
	return true
}

func (d *NotificationDispatcher) forget(ctx context.Context, key domain.NotificationThrottleKey) {
	if d.throttles != nil {
		if err := d.throttles.Clear(ctx, key); err != nil {
			slog.Error("notification dispatcher: clear throttle failed", "monitor_id", key.MonitorID, "error", err)
		}
		return
	}
	d.mu.Lock()
	delete(d.lastNotified, key)
	d.mu.Unlock()
}
