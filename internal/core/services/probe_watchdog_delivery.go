package services

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeWatchdogDeliveryService sends source watchdog work with explicit probe
// context. The edge dispatcher delegates its claimed watchdog items here; the
// hub runs it within the stable parent owner. Mirrors never invoke this service.
type ProbeWatchdogDeliveryService struct {
	outbox     ports.DeliveryOutboxRepository
	configs    ports.EdgeConfigReader
	authorizer ports.ProbeWatchdogDeliveryRepository
	authority  func(context.Context) (domain.ProbeWatchdogAuthority, error)
	sender     func(string) (ports.NotificationSender, bool)
	now        func() time.Time
}

// NewProbeWatchdogDeliveryService wires both source adapters to one send policy.
func NewProbeWatchdogDeliveryService(outbox ports.DeliveryOutboxRepository, configs ports.EdgeConfigReader, authorizer ports.ProbeWatchdogDeliveryRepository, authority func(context.Context) (domain.ProbeWatchdogAuthority, error), sender func(string) (ports.NotificationSender, bool)) (*ProbeWatchdogDeliveryService, error) {
	if outbox == nil || configs == nil || authorizer == nil || authority == nil || sender == nil {
		return nil, domain.ErrValidation
	}
	return &ProbeWatchdogDeliveryService{outbox: outbox, configs: configs, authorizer: authorizer, authority: authority, sender: sender, now: time.Now}, nil
}

// Process rechecks the exact applied graph and DB authority before a bounded send.
// The immutable delivery returned by storage, not the caller's snapshot, renders it.
func (s *ProbeWatchdogDeliveryService) Process(ctx context.Context, item domain.QueuedDelivery) error {
	claim := domain.DeliveryClaim{ProbeID: item.ProbeID, DeliveryID: item.DeliveryID, Attempt: item.Attempt, LeaseToken: item.LeaseToken}
	finish := func(status, code string, retry time.Time) error {
		return s.outbox.FinishDelivery(ctx, claim, domain.DeliveryResult{Status: status, ErrorCode: code, At: s.now().UTC(), RetryAt: retry})
	}
	config, err := s.configs.Load(ctx)
	if err != nil {
		return err
	}
	if config == nil || config.Metadata.ProbeID != claim.ProbeID {
		return domain.ErrValidation
	}
	authority, err := s.authority(ctx)
	if err != nil {
		return err
	}
	// One deadline covers authorization, rendering and provider I/O, so time
	// spent authorizing cannot extend the lease coverage promised by storage.
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stored, err := s.authorizer.AuthorizeWatchdogDelivery(sendCtx, authority, claim, config.Metadata, 10*time.Second)
	if err != nil {
		return err
	}
	if stored == nil || !config.Watchdog.Enabled {
		return finish(domain.DeliveryStatusSuperseded, "", time.Time{})
	}
	channel, ok := config.Channels[stored.NotificationID]
	if !slices.Contains(config.Watchdog.NotificationIDs, stored.NotificationID) || !ok || channel.Notification == nil || !channel.Notification.Active || channel.Notification.ID != stored.NotificationID || channel.Version != stored.NotificationVersion {
		return finish(domain.DeliveryStatusSuperseded, "", time.Time{})
	}
	if stored.EventKind != domain.DeliveryEventProbeConnection || stored.MonitorID != 0 || stored.AssignmentGeneration != 0 || stored.ProbeID != config.Metadata.ProbeID || config.Probe.Name == "" {
		return domain.ErrValidation
	}
	provider, ok := s.sender(channel.Notification.Type)
	if !ok || provider == nil {
		return finish(domain.DeliveryStatusFailed, domain.ErrCodeUnknownSenderType, time.Time{})
	}
	alert := probeWatchdogAlertContext(*stored, config, channel.Notification, s.now().UTC())
	if err := sendCtx.Err(); err != nil {
		return err
	}
	err = provider.Send(sendCtx, channel.Notification.Config, alert)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		err = sendCtx.Err()
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

func probeWatchdogAlertContext(item domain.QueuedDelivery, config *domain.EdgeResolvedConfig, channel *domain.Notification, at time.Time) domain.AlertContext {
	end := at
	if item.ResolvedAt != nil {
		end = *item.ResolvedAt
	}
	alert := domain.AlertContext{AlertScope: domain.AlertScopeProbe, DeliveryScope: domain.IncidentScopeProbeConnection, SourceAlertID: item.SourceAlertID, ProbeID: item.ProbeID, ProbeName: config.Probe.Name, ProbeLocation: config.Probe.Location, Status: item.CheckStatus, PreviousStatus: domain.StatusUp, EventKind: domain.DeliveryEventProbeConnection, Message: item.CheckOutput, CheckOutput: item.CheckOutput, StartedAt: item.StartedAt.UTC(), Duration: max(time.Duration(0), end.Sub(item.StartedAt))}
	if item.CheckStatus == domain.StatusUp {
		alert.PreviousStatus = domain.StatusDown
	}
	if channel.TemplateID != nil {
		if template := config.Templates[*channel.TemplateID]; template != nil && template.Provider == channel.Type {
			alert.TemplateTitle, alert.TemplateBody, alert.TemplateConfig = template.TitleTemplate, template.BodyTemplate, template.Config
		}
	}
	return alert
}

// Run is used only for the hub's source-owned probe outbox. The edge already has
// one dispatcher for all local work and delegates watchdog items to Process.
func (s *ProbeWatchdogDeliveryService) Run(ctx context.Context, probeID string, report func(error)) {
	runSourceDeliveries(ctx, func(ctx context.Context) (bool, error) {
		items, err := s.outbox.ClaimDeliveries(ctx, probeID, s.now().UTC(), time.Minute, 1)
		if err != nil || len(items) == 0 {
			return false, err
		}
		return true, s.Process(ctx, items[0])
	}, report)
}

func runSourceDeliveries(ctx context.Context, process func(context.Context) (bool, error), report func(error)) {
	runSourceDeliveriesUntilQuiesced(ctx, nil, process, report)
}

func runSourceDeliveriesUntilQuiesced(ctx context.Context, quiesce <-chan struct{}, process func(context.Context) (bool, error), report func(error)) {
	for ctx.Err() == nil {
		select {
		case <-quiesce:
			return
		default:
		}
		worked, err := process(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && report != nil {
			report(err)
		}
		if worked && err == nil {
			continue
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-quiesce:
			timer.Stop()
			return
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
