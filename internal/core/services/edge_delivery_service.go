package services

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// EdgeDeliveryService sends only durable source work authorized by the current
// accepted graph. It has no hub repository or hub-reachability dependency.
type EdgeDeliveryService struct {
	watchdog *ProbeWatchdogDeliveryService
	outbox   ports.DeliveryOutboxRepository
	configs  ports.EdgeConfigReader
	checks   ports.EdgeCheckRepository
	cron     ports.CronEvaluator
	sender   func(string) (ports.NotificationSender, bool)
	now      func() time.Time
}

// NewEdgeDeliveryService wires immutable config reads and actual provider lookup.
func NewEdgeDeliveryService(outbox ports.DeliveryOutboxRepository, configs ports.EdgeConfigReader, checks ports.EdgeCheckRepository, cron ports.CronEvaluator, sender func(string) (ports.NotificationSender, bool)) *EdgeDeliveryService {
	return &EdgeDeliveryService{outbox: outbox, configs: configs, checks: checks, cron: cron, sender: sender, now: time.Now}
}

// SetWatchdog attaches probe-scoped delivery reconciliation before Run.
func (s *EdgeDeliveryService) SetWatchdog(watchdog *ProbeWatchdogDeliveryService) {
	s.watchdog = watchdog
}

// ProcessNext claims one delivery so work cannot expire while waiting behind a
// batch of slow sends. Failed persistence is returned, never reported as sent.
func (s *EdgeDeliveryService) ProcessNext(ctx context.Context, probeID string) (bool, error) {
	items, err := s.outbox.ClaimDeliveries(ctx, probeID, s.now().UTC(), time.Minute, 1)
	if err != nil || len(items) == 0 {
		return false, err
	}
	return true, s.process(ctx, items[0])
}

func (s *EdgeDeliveryService) process(ctx context.Context, item domain.QueuedDelivery) error {
	if item.EventKind == domain.DeliveryEventProbeConnection && s.watchdog != nil {
		return s.watchdog.Process(ctx, item)
	}
	claim := domain.DeliveryClaim{DeliveryID: item.DeliveryID, ProbeID: item.ProbeID, Attempt: item.Attempt, LeaseToken: item.LeaseToken}
	finish := func(status, code string, retry time.Time) error {
		return s.outbox.FinishDelivery(ctx, claim, domain.DeliveryResult{Status: status, ErrorCode: code, At: s.now().UTC(), RetryAt: retry})
	}
	config, err := s.configs.Load(ctx)
	if err != nil {
		return err
	}
	if config == nil || config.Metadata.ProbeID != item.ProbeID {
		return domain.ErrValidation
	}
	var assignment *domain.EdgeResolvedAssignment
	for n := range config.Assignments {
		a := &config.Assignments[n]
		if a.Monitor != nil && a.Monitor.ID == item.MonitorID && a.Generation == item.AssignmentGeneration && a.Monitor.Active {
			assignment = a
			break
		}
	}
	channel, exists := config.Channels[item.NotificationID]
	if assignment == nil || !exists || channel.Notification == nil || !channel.Notification.Active || channel.Version != item.NotificationVersion {
		return finish(domain.DeliveryStatusSuperseded, "", time.Time{})
	}
	linked, includeTarget := false, false
	for _, link := range assignment.NotificationLinks {
		if link.NotificationID == item.NotificationID {
			linked, includeTarget = true, link.IncludeTarget
			break
		}
	}
	maintenance, err := EdgeMaintenanceActive(config, *assignment, s.cron, s.now().UTC())
	if err != nil {
		return err
	}
	if !linked || maintenance {
		return finish(domain.DeliveryStatusSuperseded, "", time.Time{})
	}
	evidence, err := s.checks.ReadEdgeEvidence(ctx, item.MonitorID, item.AssignmentGeneration)
	if err != nil {
		return err
	}
	if item.CheckStatus == domain.StatusDown && (evidence.Incident == nil || evidence.Incident.SourceAlertID != item.SourceAlertID || evidence.Incident.Status != domain.AlertStatusFiring || evidence.Incident.TransitionVersion != item.SourceTransitionVersion) {
		return finish(domain.DeliveryStatusSuperseded, "", time.Time{})
	}
	provider, ok := s.sender(channel.Notification.Type)
	if !ok || provider == nil {
		return finish(domain.DeliveryStatusFailed, domain.ErrCodeUnknownSenderType, time.Time{})
	}
	alert := edgeAlertContext(item, *assignment, config, channel.Notification, includeTarget, s.now().UTC())
	// Re-read the persisted claim at the external-I/O boundary. A recovered or
	// superseded attempt cannot send with another worker's authority.
	stored, err := s.outbox.GetDeliveryIntent(ctx, item.ProbeID, item.DeliveryID)
	if err != nil {
		return err
	}
	if stored.Status != domain.DeliveryStatusLeased || stored.LeaseToken != item.LeaseToken || stored.Attempt != item.Attempt || stored.LeaseUntil == nil || !s.now().UTC().Add(10*time.Second).Before(*stored.LeaseUntil) {
		return ports.ErrConflict
	}
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = provider.Send(sendCtx, channel.Notification.Config, alert)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return finish(domain.DeliveryStatusSent, "", time.Time{})
	}
	code, retryable := ClassifyDeliveryError(err)
	if retryable && item.Attempt < 5 {
		return finish(domain.DeliveryStatusRetrying, code, s.now().UTC().Add(CalculateBackoff(item.Attempt)))
	}
	return finish(domain.DeliveryStatusFailed, code, time.Time{})
}

func edgeAlertContext(item domain.QueuedDelivery, a domain.EdgeResolvedAssignment, config *domain.EdgeResolvedConfig, channel *domain.Notification, includeTarget bool, at time.Time) domain.AlertContext {
	m := a.Monitor
	end := at
	if item.ResolvedAt != nil {
		end = *item.ResolvedAt
	}
	alert := domain.AlertContext{AlertScope: domain.AlertScopeMonitor, ProbeID: item.ProbeID, AssignmentGeneration: item.AssignmentGeneration, DeliveryScope: domain.IncidentScopeRegional, MonitorID: m.ID, MonitorName: m.Name, MonitorType: m.Type, MonitorTarget: m.Target(), MonitorDescription: m.Description, MonitorOwner: a.EffectiveOwner, Status: item.CheckStatus, PreviousStatus: domain.StatusUp, Message: item.CheckOutput, CheckOutput: item.CheckOutput, Duration: max(time.Duration(0), end.Sub(item.StartedAt)), StartedAt: item.StartedAt.UTC(), EventKind: item.EventKind, Tags: make(map[string]string)}
	if item.CheckStatus == domain.StatusUp {
		alert.PreviousStatus = domain.StatusDown
	}
	for _, tag := range a.Tags {
		alert.Tags[tag.Name] = tag.Value
	}
	if channel.TemplateID != nil {
		if template := config.Templates[*channel.TemplateID]; template != nil && template.Provider == channel.Type {
			alert.TemplateTitle, alert.TemplateBody, alert.TemplateConfig = template.TitleTemplate, template.BodyTemplate, template.Config
		}
	}
	return applyTargetPolicy(includeTarget, alert)
}

// Run processes source work until shutdown. A bounded pause on storage failures
// avoids a busy loop; callers receive errors through the supplied diagnostic hook.
func (s *EdgeDeliveryService) Run(ctx context.Context, probeID string, report func(error)) {
	runSourceDeliveries(ctx, func(ctx context.Context) (bool, error) { return s.ProcessNext(ctx, probeID) }, report)
}
