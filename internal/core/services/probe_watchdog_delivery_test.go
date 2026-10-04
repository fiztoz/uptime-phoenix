package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type watchdogDeliveryFake struct {
	edgeDeliveryFake
	authorizeErr error
	superseded   bool
	finishErr    error
	authorized   bool
}

func (f *watchdogDeliveryFake) AuthorizeWatchdogDelivery(ctx context.Context, a domain.ProbeWatchdogAuthority, claim domain.DeliveryClaim, config domain.ProbeConfigMetadata, budget time.Duration) (*domain.QueuedDelivery, error) {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > budget || a.HealthGeneration != 0 || a.ProbeID != claim.ProbeID || claim.Attempt != f.item.Attempt || claim.LeaseToken != f.item.LeaseToken {
		return nil, ports.ErrConflict
	}
	if f.authorizeErr != nil {
		return nil, f.authorizeErr
	}
	f.authorized = true
	if f.superseded {
		return nil, nil
	}
	copy := f.item
	return &copy, nil
}
func (f *watchdogDeliveryFake) FinishDelivery(ctx context.Context, claim domain.DeliveryClaim, result domain.DeliveryResult) error {
	if f.finishErr != nil {
		return f.finishErr
	}
	return f.edgeDeliveryFake.FinishDelivery(ctx, claim, result)
}

func TestProbeWatchdogDeliveryReconcilesBeforeSend(t *testing.T) {
	for _, scenario := range []string{"sent", "disabled", "removed", "inactive", "rotated", "unlinked", "superseded", "stale owner", "stale claim", "retry", "finish failure", "template", "caller context ignored"} {
		t.Run(scenario, func(t *testing.T) {
			_, _, authority, config := watchdogSourceFixture(t)
			authority.HealthGeneration = 0
			config.Probe = domain.ProbeDisplay{Name: "Bangkok edge", Location: "TH"}
			config.Channels[1].Notification.Type = "webhook"
			at := time.Now().UTC()
			f := &watchdogDeliveryFake{edgeDeliveryFake: edgeDeliveryFake{config: config, item: domain.QueuedDelivery{DeliveryIntent: domain.DeliveryIntent{DeliveryID: "delivery", ProbeID: authority.ProbeID, SourceAlertID: "original-incident", SourceTransitionVersion: 1, NotificationID: 1, NotificationVersion: 1, EventKind: domain.DeliveryEventProbeConnection}, Status: domain.DeliveryStatusLeased, Attempt: 1, LeaseToken: "claim", CheckStatus: domain.StatusDown, StartedAt: at.Add(-time.Minute), CheckOutput: "Hub application health unavailable"}}}
			svc, err := NewProbeWatchdogDeliveryService(f, f, f, func(context.Context) (domain.ProbeWatchdogAuthority, error) { return authority, nil }, func(string) (ports.NotificationSender, bool) { return f, true })
			if err != nil {
				t.Fatal(err)
			}
			svc.now = func() time.Time { return at }
			item := f.item
			switch scenario {
			case "disabled":
				config.Watchdog.Enabled = false
			case "removed":
				delete(config.Channels, 1)
			case "inactive":
				config.Channels[1].Notification.Active = false
			case "rotated":
				channel := config.Channels[1]
				channel.Version++
				config.Channels[1] = channel
			case "unlinked":
				config.Watchdog.NotificationIDs = nil
			case "superseded":
				f.superseded = true
			case "stale owner":
				f.authorizeErr = ports.ErrConflict
			case "stale claim":
				item.LeaseToken = "old-claim"
			case "retry":
				f.fail = errors.New("private-token@example connection refused")
			case "finish failure":
				f.finishErr = ports.ErrConflict
			case "template":
				id := int64(9)
				config.Channels[1].Notification.TemplateID = &id
				config.Templates = map[int64]*domain.NotificationTemplate{9: {Provider: "webhook", TitleTemplate: "{{probe.name}}", BodyTemplate: "{{alert.source_id}}"}}
			case "caller context ignored":
				item.SourceAlertID = "forged"
				item.MonitorID = 999
				item.CheckOutput = "forged"
			}
			err = svc.Process(t.Context(), item)
			if scenario == "stale owner" || scenario == "stale claim" {
				if !errors.Is(err, ports.ErrConflict) || len(f.sent) != 0 || f.result.Status != "" {
					t.Fatal("stale authority performed work", err)
				}
				return
			}
			if scenario == "finish failure" {
				if !errors.Is(err, ports.ErrConflict) || len(f.sent) != 1 || f.result.Status != "" {
					t.Fatal("persistence failure reported sent", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "disabled" || scenario == "removed" || scenario == "inactive" || scenario == "rotated" || scenario == "unlinked" || scenario == "superseded" {
				if len(f.sent) != 0 || f.result.Status != domain.DeliveryStatusSuperseded {
					t.Fatal("ineligible provider I/O")
				}
				return
			}
			if !f.authorized || len(f.sent) != 1 {
				t.Fatal("send lacked DB authorization")
			}
			alert := f.sent[0]
			if alert.AlertScope != domain.AlertScopeProbe || alert.DeliveryScope != domain.IncidentScopeProbeConnection || alert.ProbeName != "Bangkok edge" || alert.ProbeLocation != "TH" || alert.SourceAlertID != "original-incident" || alert.MonitorID != 0 || alert.MonitorName != "" || alert.MonitorType != "" || alert.AssignmentGeneration != 0 || alert.AckURL != "" || alert.Message != f.item.CheckOutput {
				t.Fatal("fabricated or lost probe context", alert)
			}
			if scenario == "template" && (alert.TemplateTitle != "{{probe.name}}" || alert.TemplateBody != "{{alert.source_id}}") {
				t.Fatal("lost probe template")
			}
			if scenario == "retry" {
				if f.result.Status != domain.DeliveryStatusRetrying || f.result.ErrorCode != domain.ErrCodeConnectionRefused || !f.result.RetryAt.After(at) {
					t.Fatal("unsafe retry", f.result)
				}
			} else if f.result.Status != domain.DeliveryStatusSent {
				t.Fatal("outcome not stored")
			}
		})
	}
}

func TestEdgeDeliveryDispatchesProbeWithoutMonitor(t *testing.T) {
	_, _, a, c := watchdogSourceFixture(t)
	a.HealthGeneration = 0
	c.Probe = domain.ProbeDisplay{Name: "Edge"}
	c.Channels[1].Notification.Type = "webhook"
	f := &watchdogDeliveryFake{edgeDeliveryFake: edgeDeliveryFake{config: c, item: domain.QueuedDelivery{DeliveryIntent: domain.DeliveryIntent{ProbeID: a.ProbeID, DeliveryID: "delivery", NotificationID: 1, NotificationVersion: 1, EventKind: domain.DeliveryEventProbeConnection}, Attempt: 1, LeaseToken: "claim", CheckStatus: domain.StatusUp}}}
	sender := func(string) (ports.NotificationSender, bool) { return f, true }
	watchdog, err := NewProbeWatchdogDeliveryService(f, f, f, func(context.Context) (domain.ProbeWatchdogAuthority, error) { return a, nil }, sender)
	if err != nil {
		t.Fatal(err)
	}
	edge := NewEdgeDeliveryService(f, f, nil, nil, sender)
	edge.SetWatchdog(watchdog)
	worked, err := edge.ProcessNext(t.Context(), a.ProbeID)
	if err != nil || !worked || len(f.sent) != 1 || f.sent[0].Status != domain.StatusUp || f.result.Status != domain.DeliveryStatusSent {
		t.Fatal("probe item went through monitor-only reconciliation", err)
	}
}
