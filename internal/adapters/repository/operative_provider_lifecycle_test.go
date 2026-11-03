package repository_test

import (
	"context"
	"errors"
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
			t.Run("BusyQuiesceKeepsRegionalPaging", func(t *testing.T) { testBusyQuiesceKeepsRegionalPaging(t, engine) })
		})
	}
}

// testBusyQuiesceKeepsRegionalPaging reproduces issue #191 on the hub-local
// source through the production heartbeat, outbox consumer, provider journal
// and cutover coordinator: an earlier regional send failed (ambiguous, which
// nothing ever clears), so the aggregate cutover's quiesce is refused busy and
// rearmed forever. The monitor must keep its regional page path meanwhile —
// before the fix the refusal kept the aggregate hold and the next DOWN paged
// nobody, while aggregate paging could never start either.
func testBusyQuiesceKeepsRegionalPaging(t *testing.T, engine string) {
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	o.p.consumer.SetProviderAuthority(o.store, o.store)
	o.p.sender.err = errors.New("dial tcp 203.0.113.7:443: connect: connection refused")
	o.record(t, domain.StatusDown)
	o.p.send(t)
	if attempts := o.attempts(t); len(attempts) != 1 || attempts[0].State != string(domain.ProviderAttemptAmbiguous) {
		t.Fatal("failed regional send did not leave one ambiguous attempt", attempts)
	}
	failed := len(o.p.sender.alerts)

	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("barrier did not reach quiescing: %+v", got)
	}
	if log := o.quiesceLog(t); len(log) != 1 || log[0].Status != "rejected" || log[0].Code != "delivery_provider_busy" {
		t.Fatalf("local refusal = %+v, want rejected delivery_provider_busy", log)
	}
	if o.suppressed(t) || o.gateCount(t, "held") != 0 {
		t.Fatal("a refused quiesce engaged the aggregate source hold")
	}

	// The provider recovers; recovery and a fresh outage still page regionally.
	o.p.sender.err = nil
	o.record(t, domain.StatusUp)
	o.p.send(t)
	// The coordinator rearms the same barrier with a new immutable command; the
	// permanent ambiguity refuses it again and still must not hold.
	backdateOperativeOutcomes(t, o.p.f.db, domain.DeliveryQuiesceKind)
	if got := o.run(t, true); got.Phase != "quiescing" || !got.Changed {
		t.Fatalf("busy refusal did not rearm: %+v", got)
	}
	if log := o.quiesceLog(t); len(log) != 2 || log[1].Code != "delivery_provider_busy" {
		t.Fatalf("rearmed quiesce = %+v, want a second busy refusal", log)
	}
	if o.suppressed(t) || o.gateCount(t, "held") != 0 {
		t.Fatal("a rearmed refused quiesce engaged the aggregate source hold")
	}
	o.record(t, domain.StatusDown)
	o.p.send(t)
	sent := o.p.sender.alerts[failed:]
	if len(sent) != 2 || sent[0].Status != domain.StatusUp || sent[1].Status != domain.StatusDown {
		t.Fatalf("regional pages after a refused quiesce = %d, want recovery then DOWN", len(sent))
	}
	var ambiguous int
	for _, attempt := range o.attempts(t) {
		if attempt.State == string(domain.ProviderAttemptAmbiguous) {
			ambiguous++
		}
	}
	if ambiguous != 1 {
		t.Fatal("ambiguous attempt was rewritten or duplicated", ambiguous)
	}
	if got := o.state(t); got.Phase != "quiescing" {
		t.Fatal("refused barrier advanced without quiescence", got.Phase)
	}
}
