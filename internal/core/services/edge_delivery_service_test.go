package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type edgeDeliveryFake struct {
	item                      domain.QueuedDelivery
	config                    *domain.EdgeResolvedConfig
	result                    domain.DeliveryResult
	sent                      []domain.AlertContext
	fail                      error
	supersededAtAuthorization bool
}

func (f *edgeDeliveryFake) ClaimDeliveries(context.Context, string, time.Time, time.Duration, int) ([]domain.QueuedDelivery, error) {
	return []domain.QueuedDelivery{f.item}, nil
}
func (f *edgeDeliveryFake) GetDeliveryIntent(context.Context, string, string) (*domain.QueuedDelivery, error) {
	copy := f.item
	return &copy, nil
}
func (f *edgeDeliveryFake) AuthorizeEdgeDelivery(ctx context.Context, claim domain.DeliveryClaim, _ domain.ProbeConfigMetadata, budget time.Duration) (*domain.QueuedDelivery, error) {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > budget || f.item.LeaseUntil == nil || !time.Now().Add(budget).Before(*f.item.LeaseUntil) || f.item.LeaseToken != claim.LeaseToken || f.item.Attempt != claim.Attempt {
		return nil, ports.ErrConflict
	}
	if f.supersededAtAuthorization {
		return nil, nil
	}
	item := f.item
	return &item, nil
}
func (f *edgeDeliveryFake) FinishDelivery(_ context.Context, _ domain.DeliveryClaim, r domain.DeliveryResult) error {
	f.result = r
	return nil
}
func (f *edgeDeliveryFake) Load(context.Context) (*domain.EdgeResolvedConfig, error) {
	return f.config, nil
}
func (*edgeDeliveryFake) Type() string                  { return "webhook" }
func (*edgeDeliveryFake) Validate(map[string]any) error { return nil }
func (f *edgeDeliveryFake) Send(ctx context.Context, _ map[string]any, a domain.AlertContext) error {
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("missing provider deadline")
	}
	f.sent = append(f.sent, a)
	return f.fail
}

func TestEdgeDeliveryServiceReconcilesAcceptedGraphAndRedactsErrors(t *testing.T) {
	for _, scenario := range []string{"sent", "retry", "removed", "maintenance", "recovered", "stale lease", "template", "ACK before authorization"} {
		t.Run(scenario, func(t *testing.T) {
			source, config, a := edgeServiceFixture()
			at := time.Now().UTC()
			until := at.Add(time.Minute)
			a.Monitor.Name = "Checkout"
			a.Monitor.Config = map[string]any{"url": "https://secret.example"}
			a.EffectiveOwner = "Operations"
			a.Tags = []domain.ProbeConfigTag{{Name: "team", Value: "ops"}}
			config.Assignments[0] = a
			config.Channels[7] = domain.EdgeResolvedChannel{Notification: &domain.Notification{ID: 7, Active: true, Type: "webhook"}, Version: 3}
			source.e.Incident = &domain.RegionalIncident{SourceAlertID: "incident", Status: domain.AlertStatusFiring, TransitionVersion: 1}
			f := &edgeDeliveryFake{config: config, item: domain.QueuedDelivery{DeliveryIntent: domain.DeliveryIntent{DeliveryID: "delivery", ProbeID: "probe", SourceAlertID: "incident", SourceTransitionVersion: 1, NotificationID: 7, NotificationVersion: 3}, MonitorID: 1, AssignmentGeneration: 2, CheckStatus: domain.StatusDown, Status: domain.DeliveryStatusLeased, Attempt: 1, LeaseToken: "claim", LeaseUntil: &until, StartedAt: at.Add(-time.Minute)}}
			svc := NewEdgeDeliveryService(f, f, source, nil, func(string) (ports.NotificationSender, bool) { return f, true })
			svc.now = func() time.Time { return at }
			switch scenario {
			case "ACK before authorization":
				f.supersededAtAuthorization = true
			case "retry":
				f.fail = errors.New("dial secret-token@example: connection refused")
			case "removed":
				delete(config.Channels, 7)
			case "maintenance":
				config.Assignments[0].MaintenanceIDs = []int64{9}
				config.Maintenance[9] = &domain.MaintenanceWindow{ID: 9, Active: true, Strategy: "single", StartDate: at.Add(-time.Second), EndDate: until}
			case "recovered":
				source.e.Incident.Status = domain.AlertStatusResolved
			case "stale lease":
				expired := at.Add(-time.Second)
				f.item.LeaseUntil = &expired
			case "template":
				id := int64(8)
				config.Channels[7].Notification.TemplateID = &id
				config.Templates = map[int64]*domain.NotificationTemplate{8: {ID: 8, Provider: "webhook", TitleTemplate: "title", BodyTemplate: "body", Config: map[string]any{"key": "value"}}}
			}
			_, err := svc.ProcessNext(t.Context(), "probe")
			if scenario == "stale lease" {
				if !errors.Is(err, ports.ErrConflict) || len(f.sent) != 0 || f.result.Status != "" {
					t.Fatal("expired attempt performed I/O")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "removed" || scenario == "maintenance" || scenario == "recovered" || scenario == "ACK before authorization" {
				if len(f.sent) != 0 || f.result.Status != domain.DeliveryStatusSuperseded {
					t.Fatal("ineligible intent sent")
				}
				return
			}
			if len(f.sent) != 1 {
				t.Fatal("provider not called")
			}
			alert := f.sent[0]
			if alert.MonitorOwner != "Operations" || alert.Tags["team"] != "ops" || alert.MonitorTarget != "" || alert.ProbeID != "probe" || alert.AckURL != "" {
				t.Fatalf("wrong notification context: %+v", alert)
			}
			if scenario == "template" && (alert.TemplateTitle != "title" || alert.TemplateBody != "body") {
				t.Fatal("template dropped")
			}
			if scenario == "retry" {
				if f.result.Status != domain.DeliveryStatusRetrying || f.result.ErrorCode != domain.ErrCodeConnectionRefused || !f.result.RetryAt.After(at) {
					t.Fatalf("wrong redacted retry: %+v", f.result)
				}
			} else if f.result.Status != domain.DeliveryStatusSent {
				t.Fatal("send outcome lost")
			}
		})
	}
}
