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
)

type rejectedLocalChannelEncoder struct{ probe.LocalConfigEncoder }

func (rejectedLocalChannelEncoder) SameLocalDeliveryChannel([]byte, domain.LocalProbeConfigDefinition, int64) (bool, error) {
	return false, errors.New("private decoder diagnostic")
}

func TestLocalDeliveryConfigFinalFence(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, scenario := range []string{"stale document", "rotated channel", "paused monitor", "comparison failure", "corrupt origin"} {
				t.Run(scenario, func(t *testing.T) {
					p := newReviewPipeline(t, engine, true)
					protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{37}, 32))
					if err != nil {
						t.Fatal(err)
					}
					encoder := probe.LocalConfigEncoder{}
					reader := repository.NewProbeActivationStore(p.f.db, encoder, protector)
					authority := repository.NewOperativeDeliveryStore(p.f.db, probe.DeliveryCutoverCodec{}, protector, nil, encoder, protector)
					p.record(t, domain.StatusDown)
					claimed, err := p.outbox.ClaimDeliveries(t.Context(), domain.LocalProbeID, time.Now().UTC(), 5*time.Minute, 10)
					if err != nil || len(claimed) != 1 {
						t.Fatalf("claim count=%d err=%v", len(claimed), err)
					}
					item := claimed[0]
					p.monitor.Description = "unrelated edit"
					if err := p.repos.MonitorRepo.Update(t.Context(), p.monitor); err != nil {
						t.Fatal(err)
					}
					active, err := refreshService(t, p.f, p.f.db).Refresh(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if got, err := reader.ReadAppliedLocalDelivery(t.Context(), item.NotificationID, item.NotificationVersion); err != nil || got == nil {
						t.Fatalf("observational read refused unchanged channel: %v", err)
					}
					switch scenario {
					case "stale document":
						p.monitor.Description = "second edit after observation"
						if err := p.repos.MonitorRepo.Update(t.Context(), p.monitor); err != nil {
							t.Fatal(err)
						}
						if _, err := refreshService(t, p.f, p.f.db).Refresh(t.Context()); err != nil {
							t.Fatal(err)
						}
					case "rotated channel":
						if _, err := p.f.db.ExecContext(t.Context(), "UPDATE notifications SET config = ? WHERE id = ?", `{"url":"https://changed.example.test"}`, item.NotificationID); err != nil {
							t.Fatal(err)
						}
						active, err = refreshService(t, p.f, p.f.db).Refresh(t.Context())
						if err != nil {
							t.Fatal(err)
						}
					case "paused monitor":
						p.monitor.Active = false
						if err := p.repos.MonitorRepo.Update(t.Context(), p.monitor); err != nil {
							t.Fatal(err)
						}
						active, err = refreshService(t, p.f, p.f.db).Refresh(t.Context())
						if err != nil {
							t.Fatal(err)
						}
					case "comparison failure":
						authority = repository.NewOperativeDeliveryStore(p.f.db, probe.DeliveryCutoverCodec{}, protector, nil, rejectedLocalChannelEncoder{}, protector)
					case "corrupt origin":
						if _, err := p.f.db.ExecContext(t.Context(), "UPDATE probe_config_snapshots SET protected_payload = ? WHERE probe_id = ? AND revision = ?", append([]byte{1}, bytes.Repeat([]byte{0}, 63)...), domain.LocalProbeID, item.NotificationVersion); err != nil {
							t.Fatal(err)
						}
					}
					claim := domain.DeliveryClaim{DeliveryID: item.DeliveryID, ProbeID: domain.LocalProbeID, Attempt: item.Attempt, LeaseToken: item.LeaseToken}
					grant, err := authority.AuthorizeLocalDelivery(t.Context(), claim, *active, 10*time.Second)
					if scenario == "comparison failure" && !errors.Is(err, domain.ErrInternal) {
						t.Fatal("private decoder failure crossed the repository boundary")
					}
					if err == nil || grant != nil {
						t.Fatal("final authorization trusted stale observational evidence")
					}
					var count int
					if err := p.f.db.NewSelect().Table("operative_provider_attempts").ColumnExpr("COUNT(*)").Scan(t.Context(), &count); err != nil || count != 0 {
						t.Fatalf("refused work journaled a provider grant: count=%d err=%v", count, err)
					}
				})
			}
		})
	}
}
