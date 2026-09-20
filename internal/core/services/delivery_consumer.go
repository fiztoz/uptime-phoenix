package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// DeliveryConsumerConfig holds tunable parameters for outbox delivery processing.
type DeliveryConsumerConfig struct {
	LeaseDuration time.Duration // default 5m (bounded between 1s and 15m)
	MaxAttempts   int64         // default 5
	BatchLimit    int           // default 50
	PublicURL     string        // optional, for AckURL
}

// DefaultDeliveryConsumerConfig returns safe default settings.
func DefaultDeliveryConsumerConfig() DeliveryConsumerConfig {
	return DeliveryConsumerConfig{
		LeaseDuration: 5 * time.Minute,
		MaxAttempts:   5,
		BatchLimit:    50,
	}
}

// ReconciliationAction represents the decision made immediately before provider I/O.
type ReconciliationAction int

const (
	ReconcileSendNormal ReconciliationAction = iota
	ReconcileSendDelayedSummary
	ReconcileSupersede
	ReconcileDropExpired
)

// ReconciliationDecision carries the action and an explanatory reason.
type ReconciliationDecision struct {
	Action ReconciliationAction
	Reason string
	Config *domain.LocalProbeConfigDefinition // Captured settings matching the applied hash.
}

// DeliveryOutboxConsumer processes due delivery intents from the regional outbox.
// It executes pre-send reconciliation immediately before calling provider I/O.
type DeliveryOutboxConsumer struct {
	outbox        ports.DeliveryOutboxRepository
	assignments   ports.MonitorProbeAssignmentRepository
	activations   ports.ProbeConfigActivationRepository
	incidents     ports.ProbeIncidentRepository
	notifs        ports.NotificationRepository
	monitorNotifs ports.MonitorNotificationRepository
	monitors      ports.MonitorRepository
	alerts        ports.AlertRepository
	templates     ports.NotificationTemplateRepository
	maintenance   maintenanceChecker
	senders       map[string]ports.NotificationSender
	tagReader     NotificationTagReader
	cfg           DeliveryConsumerConfig
	now           func() time.Time
}

// NewDeliveryOutboxConsumer creates a new outbox delivery consumer.
func NewDeliveryOutboxConsumer(
	outbox ports.DeliveryOutboxRepository,
	notifs ports.NotificationRepository,
	cfg DeliveryConsumerConfig,
) *DeliveryOutboxConsumer {
	if cfg.LeaseDuration < time.Second || cfg.LeaseDuration > 15*time.Minute {
		cfg.LeaseDuration = 5 * time.Minute
	}
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 5
	}
	if cfg.BatchLimit < 1 || cfg.BatchLimit > 100 {
		cfg.BatchLimit = 50
	}
	return &DeliveryOutboxConsumer{
		outbox:  outbox,
		notifs:  notifs,
		senders: make(map[string]ports.NotificationSender),
		cfg:     cfg,
		now:     time.Now,
	}
}

// SetAssignmentRepository attaches assignment tracking for generation fences.
func (c *DeliveryOutboxConsumer) SetAssignmentRepository(repo ports.MonitorProbeAssignmentRepository) {
	c.assignments = repo
}

// SetActivationRepository attaches active configuration revision tracking.
func (c *DeliveryOutboxConsumer) SetActivationRepository(repo ports.ProbeConfigActivationRepository) {
	c.activations = repo
}

// SetIncidentRepository attaches incident lifecycle tracking.
func (c *DeliveryOutboxConsumer) SetIncidentRepository(repo ports.ProbeIncidentRepository) {
	c.incidents = repo
}

// SetMonitorNotificationRepository attaches monitor-channel link verification.
func (c *DeliveryOutboxConsumer) SetMonitorNotificationRepository(repo ports.MonitorNotificationRepository) {
	c.monitorNotifs = repo
}

// SetMonitorRepository attaches monitor metadata lookup.
func (c *DeliveryOutboxConsumer) SetMonitorRepository(repo ports.MonitorRepository) {
	c.monitors = repo
}

// SetAlertRepository attaches alert entity lookup (for ack tokens).
func (c *DeliveryOutboxConsumer) SetAlertRepository(repo ports.AlertRepository) {
	c.alerts = repo
}

// SetTemplateRepository attaches reusable notification templates.
func (c *DeliveryOutboxConsumer) SetTemplateRepository(repo ports.NotificationTemplateRepository) {
	c.templates = repo
}

// SetMaintenanceChecker attaches active maintenance window suppression.
func (c *DeliveryOutboxConsumer) SetMaintenanceChecker(m maintenanceChecker) {
	c.maintenance = m
}

// SetTagReader attaches monitor tag metadata.
func (c *DeliveryOutboxConsumer) SetTagReader(reader NotificationTagReader) {
	c.tagReader = reader
}

// RegisterSender registers a notification sender for a specific provider type.
func (c *DeliveryOutboxConsumer) RegisterSender(sender ports.NotificationSender) {
	c.senders[sender.Type()] = sender
}

// ProcessBatch claims due deliveries for probeID and reconciles/sends each.
// Returns the count of processed deliveries.
func (c *DeliveryOutboxConsumer) ProcessBatch(ctx context.Context, probeID string, at time.Time, limit int) (int, error) {
	if limit <= 0 || limit > c.cfg.BatchLimit {
		limit = c.cfg.BatchLimit
	}
	claimed, err := c.outbox.ClaimDeliveries(ctx, probeID, at, c.cfg.LeaseDuration, limit)
	if err != nil {
		return 0, fmt.Errorf("delivery consumer claim: %w", err)
	}
	processed := 0
	for _, delivery := range claimed {
		if err := c.ProcessOne(ctx, delivery); err != nil {
			slog.Error("delivery consumer process error",
				"delivery_id", delivery.DeliveryID,
				"monitor_id", delivery.MonitorID,
				"error", err)
		}
		processed++
	}
	return processed, nil
}

// ProcessOne performs pre-send reconciliation, executes provider I/O if valid,
// and atomically records the final outcome via FinishDelivery.
func (c *DeliveryOutboxConsumer) ProcessOne(ctx context.Context, delivery domain.QueuedDelivery) error {
	now := c.now().UTC()
	claim := domain.DeliveryClaim{
		DeliveryID: delivery.DeliveryID,
		ProbeID:    delivery.ProbeID,
		Attempt:    delivery.Attempt,
		LeaseToken: delivery.LeaseToken,
	}

	decision, notif, monitor, err := c.ReconcileBeforeSend(ctx, delivery)
	if err != nil {
		return fmt.Errorf("reconciliation error: %w", err)
	}

	switch decision.Action {
	case ReconcileDropExpired:
		// Lease expired before we could process. Discard without side effects.
		slog.Warn("delivery consumer: lease expired, dropping attempt",
			"delivery_id", delivery.DeliveryID,
			"attempt", delivery.Attempt)
		return nil

	case ReconcileSupersede:
		// Invariant failed (stale generation, removed channel, maintenance, acked resend).
		slog.Info("delivery consumer: superseding delivery",
			"delivery_id", delivery.DeliveryID,
			"reason", decision.Reason)
		result := domain.DeliveryResult{
			Status: domain.DeliveryStatusSuperseded,
			At:     now,
		}
		if finishErr := c.outbox.FinishDelivery(ctx, claim, result); finishErr != nil {
			if errors.Is(finishErr, ports.ErrConflict) {
				return nil
			}
			return fmt.Errorf("finish superseded delivery: %w", finishErr)
		}
		return nil

	case ReconcileSendDelayedSummary:
		// Recovery committed its own durable summary intent. Finish only this
		// obsolete DOWN; never perform an untracked provider send here.
		return c.outbox.FinishDelivery(ctx, claim, domain.DeliveryResult{Status: domain.DeliveryStatusSuperseded, At: c.now().UTC()})

	case ReconcileSendNormal:
		// Proceed to provider I/O.
		return c.executeSendAndFinish(ctx, claim, delivery, notif, monitor, decision.Config)
	}

	return nil
}

// ReconcileBeforeSend verifies all pre-send invariants immediately before provider I/O.
func (c *DeliveryOutboxConsumer) ReconcileBeforeSend(
	ctx context.Context,
	delivery domain.QueuedDelivery,
) (ReconciliationDecision, *domain.Notification, *domain.Monitor, error) {
	now := c.now().UTC()

	// 1. Check claim authority & lease expiry.
	stored, err := c.outbox.GetDeliveryIntent(ctx, delivery.ProbeID, delivery.DeliveryID)
	if err != nil {
		return ReconciliationDecision{}, nil, nil, err
	}
	if stored.Status != domain.DeliveryStatusLeased || stored.Attempt != delivery.Attempt || stored.LeaseToken != delivery.LeaseToken {
		return ReconciliationDecision{Action: ReconcileDropExpired, Reason: "claim_superseded"}, nil, nil, nil
	}
	if stored.LeaseUntil == nil || !now.Before(stored.LeaseUntil.UTC()) {
		return ReconciliationDecision{Action: ReconcileDropExpired, Reason: "lease_expired"}, nil, nil, nil
	}

	// 2. Check assignment generation and active state.
	if c.assignments != nil {
		set, err := c.assignments.GetByMonitorID(ctx, delivery.MonitorID)
		if errors.Is(err, ports.ErrNotFound) {
			return ReconciliationDecision{Action: ReconcileSupersede, Reason: "assignment_superseded"}, nil, nil, nil
		} else if err != nil {
			return ReconciliationDecision{}, nil, nil, err
		}
		if set == nil {
			return ReconciliationDecision{Action: ReconcileSupersede, Reason: "assignment_superseded"}, nil, nil, nil
		}
		found := false
		for _, a := range set.Assignments {
			if a.ProbeID == delivery.ProbeID && a.Generation == delivery.AssignmentGeneration {
				found = true
				break
			}
		}
		if !found {
			return ReconciliationDecision{Action: ReconcileSupersede, Reason: "assignment_superseded"}, nil, nil, nil
		}
	}

	// 3. Check active configuration & channel version.
	var applied *domain.LocalProbeConfigDefinition
	if c.activations != nil {
		active, err := c.activations.GetActive(ctx, delivery.ProbeID)
		if errors.Is(err, ports.ErrNotFound) {
			return ReconciliationDecision{Action: ReconcileSupersede, Reason: "channel_version_rotated"}, nil, nil, nil
		} else if err != nil {
			return ReconciliationDecision{}, nil, nil, err
		}
		if active == nil || active.Revision != delivery.NotificationVersion {
			// Channel version is no longer applicable. Do not send using rotated/stale credentials.
			return ReconciliationDecision{Action: ReconcileSupersede, Reason: "channel_version_rotated"}, nil, nil, nil
		}
		reader, ok := c.activations.(ports.LocalAppliedConfigReader)
		if !ok {
			return ReconciliationDecision{}, nil, nil, domain.ErrValidation
		}
		applied, err = reader.ReadAppliedLocal(ctx)
		if errors.Is(err, ports.ErrConflict) || errors.Is(err, ports.ErrNotFound) {
			return ReconciliationDecision{Action: ReconcileSupersede, Reason: "configuration_changed"}, nil, nil, nil
		}
		if err != nil {
			return ReconciliationDecision{}, nil, nil, err
		}
		if applied.Revision != delivery.NotificationVersion {
			return ReconciliationDecision{Action: ReconcileSupersede, Reason: "configuration_changed"}, nil, nil, nil
		}
	}

	// 4. Check notification channel existence and active state.
	var notif *domain.Notification
	if applied != nil {
		for _, channel := range applied.Notifications {
			if channel.ID == delivery.NotificationID {
				notif = channel
				break
			}
		}
		if notif == nil || !notif.Active {
			return ReconciliationDecision{Action: ReconcileSupersede, Reason: "channel_disabled"}, nil, nil, nil
		}
	} else if c.notifs != nil {
		var err error
		notif, err = c.notifs.GetByID(ctx, delivery.NotificationID)
		if errors.Is(err, ports.ErrNotFound) {
			return ReconciliationDecision{Action: ReconcileSupersede, Reason: "channel_removed"}, nil, nil, nil
		} else if err != nil {
			return ReconciliationDecision{}, nil, nil, err
		}
		if notif == nil {
			return ReconciliationDecision{Action: ReconcileSupersede, Reason: "channel_removed"}, nil, nil, nil
		}
		if !notif.Active {
			return ReconciliationDecision{Action: ReconcileSupersede, Reason: "channel_disabled"}, notif, nil, nil
		}
	}

	// 5. Check monitor-channel link.
	if c.monitorNotifs != nil {
		links, err := c.monitorNotifs.ListByMonitor(ctx, delivery.MonitorID)
		if err != nil {
			return ReconciliationDecision{}, nil, nil, fmt.Errorf("check monitor links: %w", err)
		}
		linked := false
		for _, link := range links {
			if link.NotificationID == delivery.NotificationID {
				linked = true
				break
			}
		}
		if !linked {
			return ReconciliationDecision{Action: ReconcileSupersede, Reason: "channel_unlinked"}, notif, nil, nil
		}
	}

	// 6. Check maintenance window.
	if c.maintenance != nil {
		inMaintenance, err := c.maintenance.IsActive(ctx, delivery.MonitorID)
		if err != nil {
			return ReconciliationDecision{}, nil, nil, err
		}
		if inMaintenance {
			return ReconciliationDecision{Action: ReconcileSupersede, Reason: "maintenance_active"}, notif, nil, nil
		}
	}

	// 7. Check incident lifecycle & acknowledgement.
	var monitor *domain.Monitor
	if applied != nil {
		for _, a := range applied.Assignments {
			if a.Monitor.ID == delivery.MonitorID && a.Generation == delivery.AssignmentGeneration {
				monitor = a.Monitor
				break
			}
		}
		if monitor == nil {
			return ReconciliationDecision{Action: ReconcileSupersede, Reason: "assignment_superseded"}, nil, nil, nil
		}
	} else if c.monitors != nil {
		monitor, err = c.monitors.GetByID(ctx, delivery.MonitorID)
		if err != nil {
			return ReconciliationDecision{}, nil, nil, err
		}
	}

	if c.incidents != nil {
		incident, err := c.incidents.GetIncident(ctx, delivery.SourceAlertID)
		if err != nil {
			return ReconciliationDecision{}, nil, nil, err
		}
		if incident != nil {
			// Recovery already queued a durable summary when needed; retire the old DOWN.
			if delivery.CheckStatus == domain.StatusDown &&
				(incident.Status == domain.AlertStatusResolved || incident.ResolvedAt != nil) {
				return ReconciliationDecision{Action: ReconcileSendDelayedSummary, Reason: "recovered_delayed_summary"}, notif, monitor, nil
			}
			// Acknowledgement suppresses DOWN, including the first claimed attempt.
			if incident.AckedAt != nil && delivery.CheckStatus == domain.StatusDown {
				if delivery.EventKind == domain.DeliveryEventIncidentSummary {
					// Summaries can still be sent.
				} else {
					// Recovery work remains eligible.
					return ReconciliationDecision{Action: ReconcileSupersede, Reason: "acknowledged_resend"}, notif, monitor, nil
				}
			}
		}
	}

	return ReconciliationDecision{Action: ReconcileSendNormal, Reason: "valid", Config: applied}, notif, monitor, nil
}

func (c *DeliveryOutboxConsumer) executeSendAndFinish(
	ctx context.Context,
	claim domain.DeliveryClaim,
	delivery domain.QueuedDelivery,
	notif *domain.Notification,
	monitor *domain.Monitor,
	applied *domain.LocalProbeConfigDefinition,
) error {
	now := c.now().UTC()

	if notif == nil {
		result := domain.DeliveryResult{
			Status:    domain.DeliveryStatusFailed,
			ErrorCode: domain.ErrCodeUnknownSenderType,
			At:        now,
		}
		return c.outbox.FinishDelivery(ctx, claim, result)
	}

	sender, ok := c.senders[notif.Type]
	if !ok {
		result := domain.DeliveryResult{
			Status:    domain.DeliveryStatusFailed,
			ErrorCode: domain.ErrCodeUnknownSenderType,
			At:        now,
		}
		return c.outbox.FinishDelivery(ctx, claim, result)
	}

	ackURL := c.resolveAckURL(ctx, delivery)
	alertContext := c.buildAlertContext(ctx, delivery, notif, monitor, ackURL, delivery.EventKind, applied)

	// NOTE on crash window:
	// If the provider accepts the send below, but the probe process crashes
	// before FinishDelivery commits, the delivery intent remains leased until
	// lease_until expires. Upon expiry, a subsequent claim may retry the send,
	// leading to an external duplicate. This window is unavoidable in distributed
	// systems without provider-side idempotency keys.
	sendErr := sender.Send(ctx, notif.Config, alertContext)
	now = c.now().UTC()

	var result domain.DeliveryResult
	if sendErr == nil {
		result = domain.DeliveryResult{
			Status: domain.DeliveryStatusSent,
			At:     now,
		}
	} else {
		code, transient := ClassifyDeliveryError(sendErr)
		if transient && delivery.Attempt < c.cfg.MaxAttempts {
			retryAt := now.Add(CalculateBackoff(delivery.Attempt))
			result = domain.DeliveryResult{
				Status:    domain.DeliveryStatusRetrying,
				ErrorCode: code,
				At:        now,
				RetryAt:   retryAt,
			}
		} else {
			result = domain.DeliveryResult{
				Status:    domain.DeliveryStatusFailed,
				ErrorCode: code,
				At:        now,
			}
		}
	}

	if err := c.outbox.FinishDelivery(ctx, claim, result); err != nil {
		if errors.Is(err, ports.ErrConflict) {
			slog.Warn("delivery consumer: finish delivery conflict (stale attempt)",
				"delivery_id", delivery.DeliveryID,
				"attempt", delivery.Attempt)
			return nil
		}
		return fmt.Errorf("finish delivery outcome: %w", err)
	}

	return nil
}

func (c *DeliveryOutboxConsumer) buildAlertContext(
	ctx context.Context,
	delivery domain.QueuedDelivery,
	notif *domain.Notification,
	monitor *domain.Monitor,
	ackURL string,
	eventKind string,
	applied *domain.LocalProbeConfigDefinition,
) domain.AlertContext {
	startedAt := delivery.StartedAt.UTC()
	var duration time.Duration
	if delivery.ResolvedAt != nil {
		duration = delivery.ResolvedAt.UTC().Sub(startedAt)
		if duration < 0 {
			duration = 0
		}
	} else if !startedAt.IsZero() {
		duration = c.now().UTC().Sub(startedAt)
		if duration < 0 {
			duration = 0
		}
	}

	monitorName := fmt.Sprintf("Monitor %d", delivery.MonitorID)
	monitorType := ""
	monitorTarget := ""
	monitorDesc := ""
	monitorOwner := ""
	if monitor != nil {
		monitorName = monitor.Name
		monitorType = monitor.Type
		monitorTarget = monitor.Target()
		monitorDesc = monitor.Description
		monitorOwner = monitor.Owner
	}

	msg := delivery.CheckOutput
	if eventKind == domain.DeliveryEventIncidentSummary {
		resolvedStr := "unknown"
		if delivery.ResolvedAt != nil {
			resolvedStr = delivery.ResolvedAt.UTC().Format(time.RFC3339)
		}
		msg = fmt.Sprintf("%s was DOWN from %s to %s (recovered)",
			monitorName, startedAt.Format(time.RFC3339), resolvedStr)
	} else if msg == "" {
		msg = fmt.Sprintf("%s is %s", monitorName, delivery.CheckStatus.String())
	}

	if ackURL != "" && delivery.CheckStatus == domain.StatusDown && eventKind != domain.DeliveryEventIncidentSummary {
		msg = msg + "\nAcknowledge: " + ackURL
	}

	tags := make(map[string]string)
	if applied != nil {
		for _, a := range applied.Assignments {
			if a.Monitor.ID == delivery.MonitorID {
				for _, tag := range a.Tags {
					tags[tag.Name] = tag.Value
				}
			}
		}
	} else if c.tagReader != nil {
		if details, err := c.tagReader.TagsForMonitor(ctx, delivery.MonitorID); err == nil {
			for _, t := range details {
				if t.Name != "" {
					tags[t.Name] = t.Value
				}
			}
		}
	}

	alert := domain.AlertContext{
		AlertScope:         domain.AlertScopeMonitor,
		MonitorID:          delivery.MonitorID,
		MonitorName:        monitorName,
		MonitorType:        monitorType,
		MonitorTarget:      monitorTarget,
		MonitorDescription: monitorDesc,
		MonitorOwner:       monitorOwner,
		Status:             delivery.CheckStatus,
		PreviousStatus:     domain.StatusUp,
		Message:            msg,
		CheckOutput:        delivery.CheckOutput,
		Duration:           duration,
		StartedAt:          startedAt,
		Tags:               tags,
		EventKind:          eventKind,
		AckURL:             ackURL,
	}

	if delivery.CheckStatus == domain.StatusUp {
		alert.PreviousStatus = domain.StatusDown
	}

	// Apply template and target policies if notif is provided.
	if notif != nil {
		alert = applyAckURLPolicy(notif, alert)
		includeTarget := domain.DefaultIncludeTarget
		if applied != nil {
			for _, a := range applied.Assignments {
				if a.Monitor.ID == delivery.MonitorID {
					for _, link := range a.NotificationLinks {
						if link.NotificationID == notif.ID {
							includeTarget = link.IncludeTarget
						}
					}
				}
			}
		} else if c.monitorNotifs != nil {
			if links, err := c.monitorNotifs.ListByMonitor(ctx, delivery.MonitorID); err == nil {
				for _, l := range links {
					if l.NotificationID == notif.ID {
						includeTarget = l.IncludeTarget
						break
					}
				}
			}
		}
		alert = applyTargetPolicy(includeTarget, alert)

		if applied != nil && notif.TemplateID != nil {
			for _, template := range applied.Templates {
				if template.ID == *notif.TemplateID && template.Provider == notif.Type {
					alert.TemplateTitle = template.TitleTemplate
					alert.TemplateBody = template.BodyTemplate
					alert.TemplateConfig = template.Config
				}
			}
		} else if notif.TemplateID != nil && c.templates != nil {
			if tmpl, err := c.templates.GetByID(ctx, *notif.TemplateID); err == nil && tmpl != nil && tmpl.Provider == notif.Type {
				alert.TemplateTitle = tmpl.TitleTemplate
				alert.TemplateBody = tmpl.BodyTemplate
				alert.TemplateConfig = tmpl.Config
			}
		}
	}

	return alert
}

func (c *DeliveryOutboxConsumer) resolveAckURL(ctx context.Context, delivery domain.QueuedDelivery) string {
	if c.cfg.PublicURL == "" || delivery.ProbeID != domain.LocalProbeID || c.alerts == nil {
		return ""
	}
	alert, err := c.alerts.GetOpenByMonitorID(ctx, delivery.MonitorID)
	if err == nil && alert != nil && alert.AckToken != "" {
		return strings.TrimRight(c.cfg.PublicURL, "/") + "/ack/" + alert.AckToken
	}
	return ""
}

// CalculateBackoff returns bounded exponential backoff for delivery retry attempts.
func CalculateBackoff(attempt int64) time.Duration {
	switch attempt {
	case 1:
		return 15 * time.Second
	case 2:
		return 1 * time.Minute
	case 3:
		return 4 * time.Minute
	case 4:
		return 15 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// ClassifyDeliveryError converts arbitrary provider errors into bounded,
// redacted snake_case diagnostic codes. It never returns raw error messages
// or strings containing potential secrets/URLs/tokens.
func ClassifyDeliveryError(err error) (code string, transient bool) {
	if err == nil {
		return "", false
	}
	s := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(s, "timeout") || strings.Contains(s, "deadline"):
		return domain.ErrCodeNetworkTimeout, true
	case strings.Contains(s, "connection refused") || strings.Contains(s, "connection reset") || strings.Contains(s, "dial"):
		return domain.ErrCodeConnectionRefused, true
	case strings.Contains(s, "rate limit") || strings.Contains(s, "too many requests") || strings.Contains(s, "429"):
		return domain.ErrCodeRateLimited, true
	case strings.Contains(s, "unauthorized") || strings.Contains(s, "forbidden") || strings.Contains(s, "401") || strings.Contains(s, "403") || strings.Contains(s, "auth"):
		return domain.ErrCodeAuthFailed, false
	case strings.Contains(s, "bad request") || strings.Contains(s, "invalid") || strings.Contains(s, "400"):
		return domain.ErrCodeBadRequest, false
	case strings.Contains(s, "500") || strings.Contains(s, "502") || strings.Contains(s, "503") || strings.Contains(s, "504") || strings.Contains(s, "bad gateway") || strings.Contains(s, "service unavailable"):
		return domain.ErrCodeProviderServerError, true
	default:
		return domain.ErrCodeProviderError, true
	}
}
