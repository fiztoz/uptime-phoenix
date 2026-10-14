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

// OperativeDeliveryService joins cutover reconciliation and genuine aggregate
// provider work. Disabled operation drains existing barriers only.
type OperativeDeliveryService struct {
	cutovers   ports.AggregateCutoverRepository
	deliveries ports.AggregateDeliveryRepository
	journal    ports.SourceProviderAttemptJournal
	owner      string
	enabled    bool
	sender     func(string) (ports.NotificationSender, bool)
}

// NewOperativeDeliveryService constructs the full opt-in runtime.
func NewOperativeDeliveryService(c ports.AggregateCutoverRepository, d ports.AggregateDeliveryRepository, j ports.SourceProviderAttemptJournal, owner string, enabled bool, sender func(string) (ports.NotificationSender, bool)) (*OperativeDeliveryService, error) {
	if c == nil || d == nil || j == nil || !domain.ValidHubID(owner) || sender == nil {
		return nil, domain.ErrValidation
	}
	return &OperativeDeliveryService{cutovers: c, deliveries: d, journal: j, owner: owner, enabled: enabled, sender: sender}, nil
}

// EvaluateAggregate reuses the pure current-evidence availability evaluator.
func (s *OperativeDeliveryService) EvaluateAggregate(in domain.AggregatePagingInput) (domain.AggregatePagingDecision, error) {
	return EvaluateAggregatePaging(in)
}

// EvaluateCutover reuses pure exact all-source application evidence.
func (s *OperativeDeliveryService) EvaluateCutover(in domain.DeliveryCutoverInput) (domain.DeliveryCutoverDecision, error) {
	return EvaluateDeliveryCutover(in)
}

// ReconcileBatch processes committed partial claims even when another failed.
func (s *OperativeDeliveryService) ReconcileBatch(ctx context.Context) (int, error) {
	leases, failed := s.cutovers.ClaimCutovers(ctx, s.owner, 30*time.Second, 20)
	n := 0
	for _, lease := range leases {
		if ctx.Err() != nil {
			return n, errors.Join(failed, ctx.Err())
		}
		if _, err := s.cutovers.ReconcileCutover(ctx, lease, s.enabled, s); err != nil {
			failed = errors.Join(failed, err)
			continue
		}
		n++
	}
	return n, failed
}

// ProcessNext admits one aggregate provider request under a ten-second budget.
func (s *OperativeDeliveryService) ProcessNext(ctx context.Context) (bool, error) {
	if !s.enabled {
		return false, nil
	}
	q, err := s.deliveries.ClaimAggregateDelivery(ctx, s.owner, 30*time.Second)
	if errors.Is(err, ports.ErrNotFound) {
		return false, nil
	}
	if err != nil || q == nil {
		return false, err
	}
	proof := q.LeaseToken
	claim := domain.DeliveryClaim{DeliveryID: q.DeliveryID, ProbeID: q.ProbeID, Attempt: q.Attempt, LeaseToken: proof}
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	grant, err := s.deliveries.AuthorizeAggregateDelivery(sendCtx, claim, 10*time.Second)
	if err != nil {
		return true, err
	}
	if grant == nil {
		return true, domain.ErrInternal
	}
	if grant.AttemptID == "" || grant.Monitor == nil || grant.Notification == nil {
		return true, domain.ErrInternal
	}
	sender, ok := s.sender(grant.Notification.Type)
	if !ok || sender == nil {
		if err := finishProviderAttemptState(ctx, s.journal, grant.AttemptID, domain.ProviderAttemptNotStarted); err != nil {
			return true, err
		}
		return true, s.deliveries.FinishAggregateDelivery(ctx, claim, domain.DeliveryResult{Status: domain.DeliveryStatusFailed, ErrorCode: domain.ErrCodeUnknownSenderType, At: time.Now().UTC()})
	}
	alert := aggregateAlertContext(*grant)
	if err := sendCtx.Err(); err != nil {
		if finishErr := finishProviderAttemptState(ctx, s.journal, grant.AttemptID, domain.ProviderAttemptNotStarted); finishErr != nil {
			return true, finishErr
		}
		return true, err
	}
	sendErr := sender.Send(sendCtx, grant.Notification.Config, alert)
	cancel()
	if err := finishProviderAttempt(ctx, s.journal, grant.AttemptID, sendErr); err != nil {
		return true, err
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	result := domain.DeliveryResult{Status: domain.DeliveryStatusSent, At: time.Now().UTC()}
	if sendErr != nil {
		code, retry := ClassifyDeliveryError(sendErr)
		result.ErrorCode, result.Status = code, domain.DeliveryStatusFailed
		if retry && q.Attempt < 5 {
			result.Status = domain.DeliveryStatusRetrying
			result.RetryAt = result.At.Add(CalculateBackoff(q.Attempt))
		}
	}
	return true, s.deliveries.FinishAggregateDelivery(ctx, claim, result)
}

func aggregateAlertContext(g domain.AggregateProviderGrant) domain.AlertContext {
	m, n, q := g.Monitor, g.Notification, g.Delivery
	status, previous := q.CheckStatus, domain.StatusUp
	if status == domain.StatusUp {
		previous = domain.StatusDown
	}
	regions := make([]string, 0, len(g.Regions))
	for _, region := range g.Regions {
		name := region.Name
		if name == "" {
			name = region.ProbeID
		}
		regions = append(regions, fmt.Sprintf("%s: %d", name, region.Status))
	}
	message := fmt.Sprintf("AGGREGATE %s (%s): %s", m.Name, g.Policy, strings.Join(regions, ", "))
	a := domain.AlertContext{
		AlertScope: domain.AlertScopeMonitor, DeliveryScope: domain.IncidentScopeAggregate,
		SourceAlertID: q.SourceAlertID, MonitorID: m.ID, MonitorName: m.Name,
		MonitorType: m.Type, MonitorTarget: m.Target(), MonitorDescription: m.Description,
		Status: status, PreviousStatus: previous, Message: message, CheckOutput: message,
		StartedAt: q.StartedAt, Duration: time.Since(q.StartedAt), EventKind: q.EventKind,
	}
	if g.Template != nil && g.Template.Provider == n.Type {
		a.TemplateTitle, a.TemplateBody, a.TemplateConfig = g.Template.TitleTemplate, g.Template.BodyTemplate, g.Template.Config
	}
	return applyTargetPolicy(g.IncludeTarget, a)
}

// Run joins reconciliation and provider work before returning on cancellation.
func (s *OperativeDeliveryService) Run(ctx context.Context, report func(error)) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if _, err := s.ReconcileBatch(ctx); err != nil && ctx.Err() == nil && report != nil {
			report(err)
		}
		if _, err := s.ProcessNext(ctx); err != nil && ctx.Err() == nil && report != nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
