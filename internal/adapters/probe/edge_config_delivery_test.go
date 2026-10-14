package probe

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type desiredModeCapturingSender struct{ alerts []domain.AlertContext }

func (*desiredModeCapturingSender) Type() string                  { return "webhook" }
func (*desiredModeCapturingSender) Validate(map[string]any) error { return nil }
func (s *desiredModeCapturingSender) Send(_ context.Context, _ map[string]any, alert domain.AlertContext) error {
	s.alerts = append(s.alerts, alert)
	return nil
}

// assertDesiredModeKeepsRegionalPaging exercises production config cold load,
// recording, SQLite commits, final outbox authorization and provider invocation.
// Only the checker result and external provider are controlled test inputs.
func assertDesiredModeKeepsRegionalPaging(t *testing.T, store *edge.Store, configs *services.EdgeConfigService, config *domain.EdgeResolvedConfig) {
	t.Helper()
	ctx := t.Context()
	a := config.Assignments[0]
	recorder := services.NewEdgeRecordingService(store, store, nil)
	sender := &desiredModeCapturingSender{}
	delivery := services.NewEdgeDeliveryService(store, configs, store, nil, func(kind string) (ports.NotificationSender, bool) {
		if kind != "webhook" {
			return nil, false
		}
		return sender, true
	})
	at := time.Now().UTC().Add(-time.Minute)
	for n := 0; n <= a.Monitor.MaxRetries; n++ {
		if _, err := recorder.Record(ctx, config, a, ports.CheckResult{Status: domain.StatusDown, Message: "controlled target failure"}, at.Add(time.Duration(n)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if worked, err := delivery.ProcessNext(ctx, config.Metadata.ProbeID); err != nil || !worked || len(sender.alerts) != 1 {
		t.Fatal("desired delivery mode suppressed the committed regional failure", err)
	}
	down := sender.alerts[0]
	if down.Status != domain.StatusDown || down.DeliveryScope != domain.IncidentScopeRegional || down.SourceAlertID == "" || down.ProbeID != config.Metadata.ProbeID || down.AssignmentGeneration != a.Generation {
		t.Fatal("regional failure lost scope or source identity")
	}
	if _, err := recorder.Record(ctx, config, a, ports.CheckResult{Status: domain.StatusUp, Message: "controlled target recovery"}, at.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if worked, err := delivery.ProcessNext(ctx, config.Metadata.ProbeID); err != nil || !worked || len(sender.alerts) != 2 {
		t.Fatal("desired delivery mode suppressed the committed regional recovery", err)
	}
	up := sender.alerts[1]
	if up.Status != domain.StatusUp || up.DeliveryScope != domain.IncidentScopeRegional || up.SourceAlertID != down.SourceAlertID {
		t.Fatal("regional recovery changed incident identity or scope")
	}
	evidence, err := store.ReadEdgeEvidence(ctx, a.Monitor.ID, a.Generation)
	if err != nil || evidence.Incident == nil || evidence.Incident.Status != domain.AlertStatusResolved || evidence.Incident.SourceAlertID != down.SourceAlertID {
		t.Fatal("provider success was not backed by the persisted regional lifecycle", err)
	}
	if worked, err := delivery.ProcessNext(ctx, config.Metadata.ProbeID); err != nil || worked || len(sender.alerts) != 2 {
		t.Fatal("completed regional lifecycle left duplicate delivery work", err)
	}
}
