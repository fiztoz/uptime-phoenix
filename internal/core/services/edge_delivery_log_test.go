package services

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TestEdgeDeliveryLogsEachOutcomeWithoutSecrets pins #125: a remote probe's
// regional alert deliveries logged nothing, so an unreachable channel was
// invisible to operators. Each finished attempt must log its outcome with ids
// and the redacted error code, never the raw provider error.
func TestEdgeDeliveryLogsEachOutcomeWithoutSecrets(t *testing.T) {
	for _, tc := range []struct {
		scenario, level, status, code string
	}{
		{"sent", "INFO", domain.DeliveryStatusSent, ""},
		{"retry", "WARN", domain.DeliveryStatusRetrying, domain.ErrCodeConnectionRefused},
		{"removed", "INFO", domain.DeliveryStatusSuperseded, ""},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			source, config, a := edgeServiceFixture()
			at := time.Now().UTC()
			until := at.Add(time.Minute)
			config.Assignments[0] = a
			config.Channels[7] = domain.EdgeResolvedChannel{Notification: &domain.Notification{ID: 7, Active: true, Type: "webhook"}, Version: 3}
			source.e.Incident = &domain.RegionalIncident{SourceAlertID: "incident", Status: domain.AlertStatusFiring, TransitionVersion: 1}
			f := &edgeDeliveryFake{config: config, item: domain.QueuedDelivery{DeliveryIntent: domain.DeliveryIntent{DeliveryID: "delivery", ProbeID: "probe", SourceAlertID: "incident", SourceTransitionVersion: 1, NotificationID: 7, NotificationVersion: 3}, MonitorID: 1, AssignmentGeneration: 2, CheckStatus: domain.StatusDown, Status: domain.DeliveryStatusLeased, Attempt: 1, LeaseToken: "claim", LeaseUntil: &until, StartedAt: at.Add(-time.Minute)}}
			switch tc.scenario {
			case "retry":
				f.fail = errors.New("dial secret-token@example: connection refused")
			case "removed":
				delete(config.Channels, 7)
			}
			var buf bytes.Buffer
			svc := NewEdgeDeliveryService(f, f, source, nil, func(string) (ports.NotificationSender, bool) { return f, true })
			svc.now = func() time.Time { return at }
			svc.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
			if _, err := svc.ProcessNext(t.Context(), "probe"); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			for _, want := range []string{"level=" + tc.level, "delivery_id=delivery", "probe_id=probe", "monitor_id=1", "notification_id=7", "attempt=1", "status=" + tc.status} {
				if !strings.Contains(out, want) {
					t.Errorf("log %q missing %q", out, want)
				}
			}
			if tc.code != "" && !strings.Contains(out, "error_code="+tc.code) {
				t.Errorf("log %q missing error_code=%s", out, tc.code)
			}
			if strings.Contains(out, "secret-token") {
				t.Errorf("log leaked the raw provider error: %q", out)
			}
		})
	}
}
