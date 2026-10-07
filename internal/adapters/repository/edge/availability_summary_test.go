package edge

import (
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestEdgeRecoverySummaryDelivery(t *testing.T) {
	for _, scenario := range []string{"pending", "retrying", "claimed", "one channel delivered"} {
		t.Run(scenario, func(t *testing.T) {
			s, dir := setupEdgeDeliveryStore(t)
			ctx := t.Context()
			config, a := certAssignment()
			a.Monitor.CertExpiryNotify = false
			a.Monitor.ResendInterval = 1
			config.Channels[11] = domain.EdgeResolvedChannel{Notification: &domain.Notification{ID: 11, Type: "webhook", Active: true}, Version: 1}
			active, err := readActiveConfig(ctx, s.db)
			if err != nil {
				t.Fatal(err)
			}
			config.Metadata = active.Snapshot.ProbeConfigMetadata
			started := time.Now().UTC().Truncate(time.Microsecond).Add(-10 * time.Minute)
			recovered := started.Add(5 * time.Minute)
			record := func(status domain.Status, at time.Time) error {
				_, err := services.NewEdgeRecordingService(s, s, nil).Record(ctx, config, a, ports.CheckResult{Status: status}, at)
				return err
			}
			if err := record(domain.StatusDown, started); err != nil {
				t.Fatal(err)
			}
			var claims []domain.QueuedDelivery
			if scenario != "pending" {
				claims, err = s.ClaimDeliveries(ctx, testIdentity().ProbeID, time.Now().UTC(), time.Minute, 10)
				if err != nil || len(claims) != 2 {
					t.Fatalf("claim: %+v %v", claims, err)
				}
				for _, item := range claims {
					if scenario == "claimed" {
						continue
					}
					status := domain.DeliveryStatusRetrying
					retry := time.Now().UTC().Add(time.Hour)
					code := domain.ErrCodeUnknownSenderType
					if scenario == "one channel delivered" && item.NotificationID == 10 {
						status = domain.DeliveryStatusSent
						retry = time.Time{}
						code = ""
					}
					claim := domain.DeliveryClaim{DeliveryID: item.DeliveryID, ProbeID: item.ProbeID, Attempt: item.Attempt, LeaseToken: item.LeaseToken}
					if err := s.FinishDelivery(ctx, claim, domain.DeliveryResult{Status: status, At: time.Now().UTC(), RetryAt: retry, ErrorCode: code}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if scenario == "one channel delivered" {
				// Exercise actual metadata cleanup with an aged sent receipt while the
				// incident remains open; the per-channel outcome must survive.
				if _, err := s.db.ExecContext(ctx, "UPDATE edge_delivery_outbox SET outcome_at = ? WHERE status = 'sent'", started.Add(-8*24*time.Hour).UnixMicro()); err != nil {
					t.Fatal(err)
				}
				if err := s.SweepRetention(ctx, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				if n := certCount(t, s, "SELECT COUNT(*) FROM edge_delivery_outbox WHERE status='sent'"); n != 1 {
					t.Fatal("retention erased open-incident delivery")
				}
			}
			// Recovery and its two channel intents must roll back together.
			if _, err := s.db.ExecContext(ctx, "CREATE TRIGGER fail_recovery BEFORE INSERT ON edge_delivery_outbox WHEN NEW.check_status = 1 BEGIN SELECT RAISE(ABORT, 'injected'); END"); err != nil {
				t.Fatal(err)
			}
			if err := record(domain.StatusUp, recovered); err == nil {
				t.Fatal("injected recovery failure accepted")
			}
			evidence, err := s.ReadEdgeEvidence(ctx, 17, 1)
			if err != nil || evidence.Incident == nil || evidence.Incident.Status != domain.AlertStatusFiring {
				t.Fatalf("partial recovery: %+v %v", evidence, err)
			}
			if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_recovery"); err != nil {
				t.Fatal(err)
			}
			if err := record(domain.StatusUp, recovered); err != nil {
				t.Fatal(err)
			}
			if err := record(domain.StatusUp, recovered.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if n := certCount(t, s, "SELECT COUNT(*) FROM edge_delivery_outbox WHERE check_status=1"); n != 2 {
				t.Fatalf("duplicate recovery intents: %d", n)
			}
			for _, item := range claims {
				if scenario != "claimed" {
					continue
				}
				claim := domain.DeliveryClaim{DeliveryID: item.DeliveryID, ProbeID: item.ProbeID, Attempt: item.Attempt, LeaseToken: item.LeaseToken}
				got, err := s.AuthorizeEdgeDelivery(ctx, claim, config.Metadata, 10*time.Second)
				if err != nil || got != nil {
					t.Fatalf("obsolete claimed DOWN authorized: %+v %v", got, err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = certStoreReopen(t, dir)
			sender := &certCapturingSender{failOnce: true}
			delivery := services.NewEdgeDeliveryService(s, certConfigReader{config}, s, nil, func(string) (ports.NotificationSender, bool) { return sender, true })
			// Make failed and previously claimed work due without waiting on wall time.
			drain := func() {
				t.Helper()
				if _, err := s.db.ExecContext(ctx, "UPDATE edge_delivery_outbox SET available_at = ?, lease_until = CASE WHEN status='leased' THEN ? ELSE lease_until END WHERE status IN ('pending','retrying','leased')", time.Now().Add(-time.Hour).UnixMicro(), time.Now().Add(-time.Second).UnixMicro()); err != nil {
					t.Fatal(err)
				}
				for range 8 {
					worked, err := delivery.ProcessNext(ctx, testIdentity().ProbeID)
					if err != nil {
						t.Fatal(err)
					}
					if !worked {
						break
					}
				}
			}
			drain()
			drain()
			if len(sender.alerts) != 2 {
				t.Fatalf("recovery sends: %+v", sender.alerts)
			}
			summaries := 0
			for _, alert := range sender.alerts {
				if alert.Status != domain.StatusUp || !alert.StartedAt.Equal(started) || alert.Duration != recovered.Sub(started) {
					t.Fatalf("lost outage identity/timing: %+v", alert)
				}
				if alert.EventKind == domain.DeliveryEventIncidentSummary {
					summaries++
					if !strings.Contains(alert.Message, started.Format(time.RFC3339Nano)) || !strings.Contains(alert.Message, recovered.Format(time.RFC3339Nano)) {
						t.Fatalf("lost original timestamps: %+v", alert)
					}
				}
			}
			want := 2
			if scenario == "one channel delivered" {
				want = 1
			}
			if summaries != want {
				t.Fatalf("summaries=%d want=%d", summaries, want)
			}
			if n := certCount(t, s, "SELECT COUNT(*) FROM edge_delivery_outbox WHERE check_status=1 AND status='sent'"); n != 2 {
				t.Fatalf("outcomes not durable: %d", n)
			}
			batch, err := s.ReadReplayBatch(ctx, 0, 64, defaultMaxBatchBytes)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range batch.Items {
				if item.Kind == "delivery.result" && strings.Contains(string(item.Payload), `"incident_summary"`) {
					found = true
				}
			}
			if !found {
				t.Fatal("summary outcome missing from ordered replay")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = certStoreReopen(t, dir)
			delivery = services.NewEdgeDeliveryService(s, certConfigReader{config}, s, nil, func(string) (ports.NotificationSender, bool) { return sender, true })
			drain()
			if len(sender.alerts) != 2 {
				t.Fatal("restart duplicated delivered summaries")
			}
		})
	}
}
