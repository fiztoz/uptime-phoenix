package repository_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestLocalDeliverySurvivesConfigChurn(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, scenario := range []string{"description", "other monitor", "before refresh", "recovery", "rotated", "rotated restored", "disabled restored", "unlinked", "paused", "corrupt origin"} {
				t.Run(scenario, func(t *testing.T) {
					p := newReviewPipeline(t, engine, true)
					protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{37}, 32))
					if err != nil {
						t.Fatal(err)
					}
					encoder := probe.LocalConfigEncoder{}
					p.consumer.SetActivationRepository(repository.NewProbeActivationStore(p.f.db, encoder, protector))
					authority := repository.NewOperativeDeliveryStore(p.f.db, probe.DeliveryCutoverCodec{}, protector, nil, encoder, protector)
					p.consumer.SetProviderAuthority(authority, authority)
					p.record(t, domain.StatusDown)
					if scenario == "recovery" {
						p.send(t)
						p.sender.alerts = nil
						p.record(t, domain.StatusUp)
					}
					claimed, err := p.outbox.ClaimDeliveries(t.Context(), domain.LocalProbeID, time.Now().UTC(), 5*time.Minute, 10)
					if err != nil || len(claimed) != 1 {
						t.Fatalf("claim count=%d err=%v", len(claimed), err)
					}
					item := claimed[0]
					refresh := func() {
						t.Helper()
						if active, err := refreshService(t, p.f, p.f.db).Refresh(t.Context()); err != nil || active.Revision <= 1 {
							t.Fatalf("refresh: active=%v err=%v", active, err)
						}
					}
					switch scenario {
					case "description", "before refresh", "recovery", "corrupt origin":
						p.monitor.Description = "description only"
						if err := p.repos.MonitorRepo.Update(t.Context(), p.monitor); err != nil {
							t.Fatal(err)
						}
					case "other monitor":
						localMonitor(t, p.f)
					case "rotated", "rotated restored":
						if _, err := p.f.db.ExecContext(t.Context(), "UPDATE notifications SET config = ? WHERE id = ?", `{"url":"https://changed.example.test"}`, item.NotificationID); err != nil {
							t.Fatal(err)
						}
					case "disabled restored":
						if _, err := p.f.db.ExecContext(t.Context(), "UPDATE notifications SET active = ? WHERE id = ?", false, item.NotificationID); err != nil {
							t.Fatal(err)
						}
					case "paused":
						p.monitor.Active = false
						if err := p.repos.MonitorRepo.Update(t.Context(), p.monitor); err != nil {
							t.Fatal(err)
						}
					case "unlinked":
						if err := p.repos.MonitorNotificationRepo.Detach(t.Context(), p.monitor.ID, item.NotificationID); err != nil {
							t.Fatal(err)
						}
					}
					if scenario == "before refresh" {
						if err := p.consumer.ProcessOne(t.Context(), item); !errors.Is(err, ports.ErrConflict) {
							t.Fatalf("source drift must remain retryable: %v", err)
						}
						stored, err := p.outbox.GetDeliveryIntent(t.Context(), domain.LocalProbeID, item.DeliveryID)
						if err != nil || stored.Status != domain.DeliveryStatusLeased || len(p.sender.alerts) != 0 {
							t.Fatal("source drift discarded work or sent before activation")
						}
					}
					refresh()
					if scenario == "rotated restored" || scenario == "disabled restored" {
						if _, err := p.f.db.ExecContext(t.Context(), "UPDATE notifications SET config = ?, active = ? WHERE id = ?", `{"url":"https://example.com"}`, true, item.NotificationID); err != nil {
							t.Fatal(err)
						}
						refresh()
					}
					if scenario == "corrupt origin" {
						if _, err := p.f.db.ExecContext(t.Context(), "UPDATE probe_config_snapshots SET protected_payload = ? WHERE probe_id = ? AND revision = ?", append([]byte{1}, bytes.Repeat([]byte{0}, 63)...), domain.LocalProbeID, item.NotificationVersion); err != nil {
							t.Fatal(err)
						}
					}
					processErr := p.consumer.ProcessOne(t.Context(), item)
					if (processErr != nil) != (scenario == "corrupt origin") {
						t.Fatalf("unexpected processing error: %v", processErr)
					}
					wantStatus, wantSends := domain.DeliveryStatusSuperseded, 0
					if scenario == "corrupt origin" {
						wantStatus = domain.DeliveryStatusLeased
					}
					if scenario == "description" || scenario == "other monitor" || scenario == "before refresh" || scenario == "recovery" {
						wantStatus, wantSends = domain.DeliveryStatusSent, 1
					}
					stored, err := p.outbox.GetDeliveryIntent(t.Context(), domain.LocalProbeID, item.DeliveryID)
					if err != nil || stored.Status != wantStatus || len(p.sender.alerts) != wantSends {
						t.Fatalf("outcome: err=%v status=%s sends=%d; want %s/%d", err, stored.Status, len(p.sender.alerts), wantStatus, wantSends)
					}
					if stored.NotificationVersion != item.NotificationVersion || stored.ConfigRevision != item.ConfigRevision {
						t.Fatal("delivery replay identity was rewritten")
					}
				})
			}
		})
	}
}
