package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

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
			// A provider error is ambiguous forever. Drain used to wait on it,
			// leaving every member held and aggregate paging stopped: the
			// monitor paged nobody (#211). It resumes once nothing can still run.
			t.Run("ProviderErrorDoesNotBlockDrain", func(t *testing.T) {
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
				if got := o.drain(t); got.Phase != "released" || o.gateCount(t, "held") != 0 {
					t.Fatal("an ambiguous send held regional paging", got.Phase)
				}
				if attempts := o.attempts(t); len(attempts) != 1 || attempts[0].State != string(domain.ProviderAttemptAmbiguous) {
					t.Fatal("drain rewrote the ambiguous attempt", attempts)
				}
			})
			t.Run("BusyQuiesceKeepsRegionalPaging", func(t *testing.T) { testBusyQuiesceKeepsRegionalPaging(t, engine) })
			t.Run("AmbiguousSendDoesNotRefuseQuiesce", func(t *testing.T) { testAmbiguousSendDoesNotRefuseQuiesce(t, engine) })
		})
	}
}

// testBusyQuiesceKeepsRegionalPaging reproduces issue #191 on the hub-local
// source through the production heartbeat, outbox consumer, provider journal
// and cutover coordinator: a send is still in progress, so the aggregate
// cutover's quiesce is refused busy and rearmed. The monitor must keep its
// regional page path meanwhile — before the fix the refusal kept the aggregate
// hold and the next DOWN paged nobody. Once the send's budget passes, the same
// barrier quiesces (#211).
func testBusyQuiesceKeepsRegionalPaging(t *testing.T, engine string) {
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	o.p.consumer.SetProviderAuthority(o.store, o.store)
	at := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := o.p.f.db.ExecContext(t.Context(), "INSERT INTO operative_provider_attempts (attempt_id,scope,monitor_id,probe_id,delivery_id,claim_attempt,notification_id,budget_until,state,created_at) VALUES (?,'source',?,'local',?,1,0,?,'in_flight',?)",
		uuid.NewString(), o.p.monitor.ID, uuid.NewString(), at.Add(time.Hour), at); err != nil {
		t.Fatal(err)
	}

	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("barrier did not reach quiescing: %+v", got)
	}
	if log := o.quiesceLog(t); len(log) != 1 || log[0].Status != "rejected" || log[0].Code != "delivery_provider_busy" {
		t.Fatalf("local refusal = %+v, want rejected delivery_provider_busy", log)
	}
	if o.suppressed(t) || o.gateCount(t, "held") != 0 {
		t.Fatal("a refused quiesce engaged the aggregate source hold")
	}
	o.record(t, domain.StatusDown)
	o.p.send(t)

	// The coordinator rearms the same barrier with a new immutable command; the
	// send still in progress refuses it again, and it still must not hold.
	backdateQuiesceOutcomes(t, o.p.f.db)
	if got := o.run(t, true); got.Phase != "quiescing" || !got.Changed {
		t.Fatalf("busy refusal did not rearm: %+v", got)
	}
	if log := o.quiesceLog(t); len(log) != 2 || log[1].Code != "delivery_provider_busy" {
		t.Fatalf("rearmed quiesce = %+v, want a second busy refusal", log)
	}
	if o.suppressed(t) || o.gateCount(t, "held") != 0 {
		t.Fatal("a rearmed refused quiesce engaged the aggregate source hold")
	}
	o.record(t, domain.StatusUp)
	o.p.send(t)
	if sent := o.p.sender.alerts; len(sent) != 2 || sent[0].Status != domain.StatusDown || sent[1].Status != domain.StatusUp {
		t.Fatalf("regional pages after a refused quiesce = %d, want DOWN then recovery", len(sent))
	}

	// The send never completes and its budget passes: it crashed. Waiting
	// longer can never resolve it, so the next quiesce succeeds.
	if _, err := o.p.f.db.ExecContext(t.Context(), "UPDATE operative_provider_attempts SET budget_until = ? WHERE state = 'in_flight'", at.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	backdateQuiesceOutcomes(t, o.p.f.db)
	for i := 0; i < 3 && o.gateCount(t, "held") == 0; i++ {
		o.run(t, true)
	}
	if !o.suppressed(t) {
		t.Fatalf("an expired in_flight send refused quiesce forever: %+v", o.quiesceLog(t))
	}
	inFlight := 0
	for _, attempt := range o.attempts(t) {
		if attempt.State == string(domain.ProviderAttemptInFlight) {
			inFlight++
		}
	}
	if inFlight != 1 {
		t.Fatalf("the crashed send's journal row was rewritten: %d in_flight", inFlight)
	}
}

// testAmbiguousSendDoesNotRefuseQuiesce: a failed regional send is ambiguous
// forever, and used to refuse every quiesce as busy, so the monitor could never
// cut over again (#211). It has already returned, so it blocks nothing.
func testAmbiguousSendDoesNotRefuseQuiesce(t *testing.T, engine string) {
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	o.p.consumer.SetProviderAuthority(o.store, o.store)
	o.p.sender.err = errors.New("dial tcp 203.0.113.7:443: connect: connection refused")
	o.record(t, domain.StatusDown)
	o.p.send(t)
	if attempts := o.attempts(t); len(attempts) != 1 || attempts[0].State != string(domain.ProviderAttemptAmbiguous) {
		t.Fatal("failed regional send did not leave one ambiguous attempt", attempts)
	}
	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("barrier did not reach quiescing: %+v", got)
	}
	if log := o.quiesceLog(t); len(log) != 1 || log[0].Status != "applied" {
		t.Fatalf("a past ambiguous send refused quiesce: %+v", log)
	}
	if !o.suppressed(t) {
		t.Fatal("a successful quiesce did not engage the aggregate source hold")
	}
	if attempts := o.attempts(t); len(attempts) != 1 || attempts[0].State != string(domain.ProviderAttemptAmbiguous) {
		t.Fatal("quiesce rewrote the ambiguous attempt", attempts)
	}
}
