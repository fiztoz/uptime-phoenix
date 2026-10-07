package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// EdgeDeliveryService sends only durable source work authorized by the current
// accepted graph. It has no hub repository or hub-reachability dependency.
type EdgeDeliveryService struct {
	watchdog *ProbeWatchdogDeliveryService
	outbox   ports.EdgeDeliveryRepository
	configs  ports.EdgeConfigReader
	checks   ports.EdgeCheckRepository
	cron     ports.CronEvaluator
	sender   func(string) (ports.NotificationSender, bool)
	now      func() time.Time
}

// NewEdgeDeliveryService wires immutable config reads and actual provider lookup.
func NewEdgeDeliveryService(outbox ports.EdgeDeliveryRepository, configs ports.EdgeConfigReader, checks ports.EdgeCheckRepository, cron ports.CronEvaluator, sender func(string) (ports.NotificationSender, bool)) *EdgeDeliveryService {
	return &EdgeDeliveryService{outbox: outbox, configs: configs, checks: checks, cron: cron, sender: sender, now: time.Now}
}

// SetWatchdog attaches probe-scoped delivery reconciliation before Run.
func (s *EdgeDeliveryService) SetWatchdog(watchdog *ProbeWatchdogDeliveryService) {
	s.watchdog = watchdog
}

// ProcessNext claims one delivery so work cannot expire while waiting behind a
// batch of slow sends. Failed persistence is returned, never reported as sent.
func (s *EdgeDeliveryService) ProcessNext(ctx context.Context, probeID string) (bool, error) {
	advanced, err := s.advanceDueEscalation(ctx, probeID)
	if err != nil || advanced {
		return advanced, err
	}
	items, err := s.outbox.ClaimDeliveries(ctx, probeID, s.now().UTC(), time.Minute, 1)
	if err != nil || len(items) == 0 {
		return false, err
	}
	return true, s.process(ctx, items[0])
}

// advanceDueEscalation commits at most one due rung. The following loop claims
// the intents it queued. A stale version means a check or acknowledgement won
// the race; the next pass reads the committed incident.
func (s *EdgeDeliveryService) advanceDueEscalation(ctx context.Context, probeID string) (bool, error) {
	if s.checks == nil {
		return false, nil
	}
	now := s.now().UTC()
	due, err := s.checks.ListDueEscalations(ctx, now, 1)
	if err != nil || len(due) == 0 {
		return false, err
	}
	config, err := s.configs.Load(ctx)
	if err != nil {
		return false, err
	}
	if config == nil || config.Metadata.ProbeID != probeID {
		return false, domain.ErrValidation
	}
	current := due[0]
	expected := current.TransitionVersion
	var assignment *domain.EdgeResolvedAssignment
	for n := range config.Assignments {
		a := &config.Assignments[n]
		if a.Monitor != nil && a.Monitor.ID == current.MonitorID && a.Generation == current.AssignmentGeneration && a.Monitor.Active {
			assignment = a
			break
		}
	}
	var policy *domain.EscalationPolicy
	if assignment != nil && assignment.EscalationPolicyID != nil && *assignment.EscalationPolicyID == current.EscalationPolicyID {
		policy = config.Policies[current.EscalationPolicyID]
	}
	step, changed := domain.AdvanceAvailabilityEscalation(&current, policy, config.Metadata.Revision, now)
	if !changed {
		return false, nil
	}
	var intents []domain.DeliveryIntent
	message := ""
	if step != nil && assignment != nil && assignment.Monitor != nil {
		message = fmt.Sprintf("ESCALATION step %d (policy %d): %s is still DOWN and unacknowledged", step.StepOrder, current.EscalationPolicyID, assignment.Monitor.Name)
		for _, id := range step.NotificationIDs {
			channel, exists := config.Channels[id]
			if !exists || channel.Notification == nil || !channel.Notification.Active || channel.Version != config.Metadata.Revision {
				continue
			}
			deliveryID, err := newUUIDv4()
			if err != nil {
				return false, err
			}
			intents = append(intents, domain.DeliveryIntent{DeliveryID: deliveryID, SourceAlertID: current.SourceAlertID, SourceTransitionVersion: current.TransitionVersion, ProbeID: probeID, NotificationID: id, NotificationVersion: channel.Version, EventKind: domain.DeliveryEventStatusChange, AvailableAt: now, EscalationPolicyID: current.EscalationPolicyID, EscalationStep: step.StepOrder})
		}
	}
	err = s.checks.CommitEscalationAdvance(ctx, expected, current, intents, message, now)
	if errors.Is(err, ports.ErrStaleLocalState) {
		return false, nil
	}
	return err == nil, err
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
	if item.EscalationStep > 0 {
		linked = escalationStepAuthorized(config, assignment, item)
		includeTarget = true
	} else {
		for _, link := range assignment.NotificationLinks {
			if link.NotificationID == item.NotificationID {
				linked, includeTarget = true, link.IncludeTarget
				break
			}
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
	// A later escalation rung keeps the incident firing and must not cancel the
	// direct notification or an earlier rung that is still leased. Acknowledgement
	// and recovery leave the firing status, which is what supersedes them.
	if item.CheckStatus == domain.StatusDown && (evidence.Incident == nil || evidence.Incident.SourceAlertID != item.SourceAlertID || evidence.Incident.Status != domain.AlertStatusFiring) {
		return finish(domain.DeliveryStatusSuperseded, "", time.Time{})
	}
	provider, ok := s.sender(channel.Notification.Type)
	if !ok || provider == nil {
		return finish(domain.DeliveryStatusFailed, domain.ErrCodeUnknownSenderType, time.Time{})
	}
	// Authorization and provider I/O share one deadline; storage delay cannot
	// consume the lease budget and then start a fresh ten-second send window.
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stored, err := s.outbox.AuthorizeEdgeDelivery(sendCtx, claim, config.Metadata, 10*time.Second)
	if err != nil {
		return err
	}
	if stored == nil {
		return finish(domain.DeliveryStatusSuperseded, "", time.Time{})
	}
	if stored.EventKind == domain.DeliveryEventCertificateExpiry && !domain.ValidEdgeCertAlertContent(stored.Certificate) {
		// Storage handed out certificate work it cannot render. Fail loudly instead
		// of sending a zero-threshold alert or silently marking it delivered.
		return fmt.Errorf("certificate delivery %s has no renderable snapshot", stored.DeliveryID)
	}
	if stored.EventKind == domain.DeliveryEventCapacityCondition && !domain.ValidEdgeConditionAlertContent(stored.Condition) {
		return fmt.Errorf("capacity delivery %s has no renderable snapshot", stored.DeliveryID)
	}
	alert := edgeAlertContext(*stored, *assignment, config, channel.Notification, includeTarget, s.now().UTC())
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

func escalationStepAuthorized(config *domain.EdgeResolvedConfig, assignment *domain.EdgeResolvedAssignment, item domain.QueuedDelivery) bool {
	if assignment.EscalationPolicyID == nil || *assignment.EscalationPolicyID != item.EscalationPolicyID {
		return false
	}
	policy := config.Policies[item.EscalationPolicyID]
	if policy == nil || !policy.Enabled {
		return false
	}
	for _, step := range policy.Steps {
		if step.StepOrder != item.EscalationStep {
			continue
		}
		for _, id := range step.NotificationIDs {
			if id == item.NotificationID {
				return true
			}
		}
	}
	return false
}

func edgeAlertContext(item domain.QueuedDelivery, a domain.EdgeResolvedAssignment, config *domain.EdgeResolvedConfig, channel *domain.Notification, includeTarget bool, at time.Time) domain.AlertContext {
	m := a.Monitor
	end := at
	if item.ResolvedAt != nil {
		end = *item.ResolvedAt
	}
	alert := domain.AlertContext{AlertScope: domain.AlertScopeMonitor, SourceAlertID: item.SourceAlertID, ProbeID: item.ProbeID, ProbeName: config.Probe.Name, ProbeLocation: config.Probe.Location, AssignmentGeneration: item.AssignmentGeneration, DeliveryScope: domain.IncidentScopeRegional, MonitorID: m.ID, MonitorName: m.Name, MonitorType: m.Type, MonitorTarget: m.Target(), MonitorDescription: m.Description, MonitorOwner: a.EffectiveOwner, Status: item.CheckStatus, PreviousStatus: domain.StatusUp, Message: item.CheckOutput, CheckOutput: item.CheckOutput, Duration: max(time.Duration(0), end.Sub(item.StartedAt)), StartedAt: item.StartedAt.UTC(), EventKind: item.EventKind, Tags: make(map[string]string)}
	if item.CheckStatus == domain.StatusUp {
		alert.PreviousStatus = domain.StatusDown
	}
	if item.EventKind == domain.DeliveryEventIncidentSummary && item.ResolvedAt != nil {
		alert.Message = fmt.Sprintf("%s was DOWN from %s to %s (recovered)", m.Name,
			item.StartedAt.UTC().Format(time.RFC3339Nano), item.ResolvedAt.UTC().Format(time.RFC3339Nano))
	}
	for _, tag := range a.Tags {
		alert.Tags[tag.Name] = tag.Value
	}
	if channel.TemplateID != nil {
		if template := config.Templates[*channel.TemplateID]; template != nil && template.Provider == channel.Type {
			alert.TemplateTitle, alert.TemplateBody, alert.TemplateConfig = template.TitleTemplate, template.BodyTemplate, template.Config
		}
	}
	if item.EventKind == domain.DeliveryEventCertificateExpiry {
		// Render from the snapshot committed with the intent, never from a value
		// re-derived at send time: a retried alert must report the threshold it was
		// actually raised for, and a later clock or renewed certificate must not
		// silently rewrite history. AuthorizeEdgeDelivery already rejected a row
		// without its snapshot, so certificate rendering never sees zero fields.
		if content := item.Certificate; domain.ValidEdgeCertAlertContent(content) {
			expiry := content.NotAfter.UTC()
			alert.EventKind = domain.AlertEventCertificateExpiry
			alert.Message = content.Message
			alert.CertThreshold = content.Threshold
			alert.CertDaysRemaining = content.DaysRemaining
			alert.CertIssuer = content.Issuer
			alert.CertNotAfter = &expiry
			// A certificate alert is not an availability transition. Pin both sides of
			// the status pair to the monitor's live state so no renderer can turn a
			// paging alert into a false "recovered" or "is DOWN" headline.
			alert.Status, alert.PreviousStatus = domain.StatusUp, domain.StatusUp
		}
	}
	if item.EventKind == domain.DeliveryEventCapacityCondition {
		// Render from the committed snapshot under the same rules: a retried
		// capacity page reports the state it was raised for, and the status pair is
		// pinned so no renderer fakes an outage or a recovery headline.
		if content := item.Condition; domain.ValidEdgeConditionAlertContent(content) {
			alert.EventKind = domain.AlertEventCapacityCondition
			alert.Message = content.Message
			alert.ConditionKind = content.Kind
			alert.ConditionState = content.State
			alert.ConditionPreviousState = content.PreviousState
			alert.ConditionUsed = content.Used
			alert.ConditionLimit = content.Limit
			alert.ConditionPercent = content.Percent
			alert.ConditionThreshold = content.Threshold
			alert.ConditionUnit = content.Unit
			alert.ConditionResource = content.Resource
			alert.ConditionScope = content.Scope
			alert.ConditionSource = content.Source
			observedAt := content.ObservedAt.UTC()
			alert.ConditionObservedAt = &observedAt
			alert.Status, alert.PreviousStatus = domain.StatusUp, domain.StatusUp
		}
	}
	return applyTargetPolicy(includeTarget, alert)
}

// Run processes source work until shutdown. A bounded pause on storage failures
// avoids a busy loop; callers receive errors through the supplied diagnostic hook.
func (s *EdgeDeliveryService) Run(ctx context.Context, probeID string, report func(error)) {
	runSourceDeliveries(ctx, func(ctx context.Context) (bool, error) { return s.ProcessNext(ctx, probeID) }, report)
}

// RunUntilQuiesced stops new claims when quiesce closes and lets an admitted
// attempt finish under ctx. Cancel ctx to bound the completion grace.
func (s *EdgeDeliveryService) RunUntilQuiesced(ctx context.Context, quiesce <-chan struct{}, probeID string, report func(error)) {
	runSourceDeliveriesUntilQuiesced(ctx, quiesce, func(ctx context.Context) (bool, error) { return s.ProcessNext(ctx, probeID) }, report)
}
