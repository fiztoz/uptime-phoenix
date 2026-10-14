package repository_test

import (
	"context"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestOperativeProviderLifecycle executes the production provider services,
// rather than treating a repository grant as a completed external request.
func TestOperativeProviderLifecycle(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
				t.Run(string(mode), func(t *testing.T) {
					o := newOperativeFixture(t, engine, mode, 1, true)
					o.p.consumer.SetProviderAuthority(o.store, o.store)
					driveOperativeActive(t, o)
					sender := &reviewSender{}
					service, err := services.NewOperativeDeliveryService(o.store, o.store, o.store, operativeOwner, true, func(string) (ports.NotificationSender, bool) { return sender, true })
					if err != nil {
						t.Fatal(err)
					}
					o.record(t, domain.StatusDown)
					o.p.send(t)
					regional := 0
					if mode == domain.AlertDeliveryBoth {
						regional = 1
					}
					if len(o.p.sender.alerts) != regional {
						t.Fatalf("regional sends = %d, want %d", len(o.p.sender.alerts), regional)
					}
					if got := o.run(t, true); !got.Changed {
						t.Fatal("fresh DOWN did not admit operative work")
					}
					if worked, err := service.ProcessNext(t.Context()); err != nil || !worked {
						t.Fatal("aggregate DOWN send", worked, err)
					}
					if len(sender.alerts) != 1 || sender.alerts[0].Status != domain.StatusDown || sender.alerts[0].DeliveryScope != domain.IncidentScopeAggregate {
						t.Fatal("wrong aggregate DOWN provider context")
					}
					for _, attempt := range o.attempts(t) {
						if attempt.State != string(domain.ProviderAttemptSucceeded) {
							t.Fatal("provider return did not complete exact grant", attempt.State)
						}
					}
					o.record(t, domain.StatusUp)
					o.p.send(t)
					if got := o.run(t, true); !got.Changed {
						t.Fatal("fresh UP did not admit recovery")
					}
					if worked, err := service.ProcessNext(t.Context()); err != nil || !worked {
						t.Fatal("aggregate recovery send", worked, err)
					}
					if len(sender.alerts) != 2 || sender.alerts[1].Status != domain.StatusUp {
						t.Fatal("recovery was rendered as DOWN")
					}
					if got := o.drain(t); got.Phase != "released" {
						t.Fatal("completed provider work did not permit safe drain", got.Phase)
					}
				})
			}
			t.Run("ProviderErrorBlocksDrain", func(t *testing.T) {
				o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
				driveOperativeActive(t, o)
				sender := &reviewSender{err: context.DeadlineExceeded}
				service, err := services.NewOperativeDeliveryService(o.store, o.store, o.store, operativeOwner, true, func(string) (ports.NotificationSender, bool) { return sender, true })
				if err != nil {
					t.Fatal(err)
				}
				o.record(t, domain.StatusDown)
				o.run(t, true)
				if worked, err := service.ProcessNext(t.Context()); err != nil || !worked {
					t.Fatal(worked, err)
				}
				if len(o.attempts(t)) != 1 || o.attempts(t)[0].State != string(domain.ProviderAttemptAmbiguous) {
					t.Fatal("provider error manufactured completion")
				}
				if got := o.drain(t); got.Phase != "draining" || o.gateCount(t, "held") != 1 {
					t.Fatal("ambiguous I/O permitted regional resume", got.Phase)
				}
			})
		})
	}
}
