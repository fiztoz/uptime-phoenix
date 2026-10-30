package services_test

import (
	"context"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestConfigAggregatePagingGate pins #119 for config-as-code: while aggregate
// paging is off, a document that asks for aggregate or both is refused in the
// plan (so Apply writes nothing at all, including the monitor of a new key),
// while a legacy stored mode that the document omits or echoes stays
// editable and moving it to regional is always allowed.
func TestConfigAggregatePagingGate(t *testing.T) {
	ctx := context.Background()
	doc := func(delivery string) *services.ConfigDocument {
		d := probeDoc()
		d.Spec.Monitors[0].AlertDelivery = delivery
		return d
	}
	refused := func(t *testing.T, svc *services.ConfigService, d *services.ConfigDocument) {
		t.Helper()
		plan, err := svc.Plan(ctx, 1, d, services.ConfigApplyOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Valid || !containsSubstring(plan.Errors, "aggregate paging is disabled") {
			t.Fatalf("plan accepted aggregate paging while disabled: %+v", plan)
		}
		res, err := svc.Apply(ctx, 1, d, services.ConfigApplyOptions{})
		if err == nil || res == nil || res.Plan == nil || res.Plan.Valid {
			t.Fatalf("apply did not refuse with the invalid plan: res=%+v err=%v", res, err)
		}
	}

	for _, mode := range []string{"aggregate", "both"} {
		t.Run("new monitor "+mode+" refused without partial writes", func(t *testing.T) {
			svc, registry, assignments, mons := newConfigProbeSvc(t)
			refused(t, svc, doc(mode))
			if len(registry.byID) != 0 || len(mons.byID) != 0 || len(assignments.sets) != 0 {
				t.Fatalf("refused document wrote state: probes=%d monitors=%d sets=%d", len(registry.byID), len(mons.byID), len(assignments.sets))
			}
		})
	}

	t.Run("existing regional set cannot move to aggregate", func(t *testing.T) {
		svc, _, assignments, mons := newConfigProbeSvc(t)
		if _, err := svc.Apply(ctx, 1, doc(""), services.ConfigApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		set := onlySet(t, assignments)
		revision := set.Revision
		d := doc("both")
		d.Spec.Monitors[0].Name = "Renamed while refused"
		refused(t, svc, d)
		if after := onlySet(t, assignments); after.Revision != revision || after.AlertDelivery == domain.AlertDeliveryBoth {
			t.Fatalf("refused document changed the set: %+v", after)
		}
		for _, m := range mons.byID {
			if m.Name != "API Health" {
				t.Fatalf("refused document renamed the monitor to %q", m.Name)
			}
		}
	})

	t.Run("legacy stored mode stays editable", func(t *testing.T) {
		svc, _, assignments, mons := newConfigProbeSvc(t)
		if _, err := svc.Apply(ctx, 1, doc(""), services.ConfigApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		set := onlySet(t, assignments)
		assignments.mu.Lock()
		assignments.sets[set.MonitorID].AlertDelivery = domain.AlertDeliveryAggregate
		assignments.mu.Unlock()

		for _, tc := range []struct{ name, delivery string }{{"omitted", ""}, {"echoed", "aggregate"}} {
			d := doc(tc.delivery)
			d.Spec.Monitors[0].Name = "Edited " + tc.name
			if _, err := svc.Apply(ctx, 1, d, services.ConfigApplyOptions{}); err != nil {
				t.Fatalf("%s legacy mode refused an unrelated edit: %v", tc.name, err)
			}
			if got := onlySet(t, assignments).AlertDelivery; got != domain.AlertDeliveryAggregate {
				t.Fatalf("%s legacy mode was rewritten to %q", tc.name, got)
			}
			for _, m := range mons.byID {
				if m.Name != d.Spec.Monitors[0].Name {
					t.Fatalf("%s edit not applied: %q", tc.name, m.Name)
				}
			}
		}
		refused(t, svc, doc("both"))
		if _, err := svc.Apply(ctx, 1, doc("regional"), services.ConfigApplyOptions{}); err != nil {
			t.Fatalf("moving a legacy set to regional: %v", err)
		}
		if got := onlySet(t, assignments).AlertDelivery; got != domain.AlertDeliveryRegional {
			t.Fatalf("legacy set did not move to regional: %q", got)
		}
	})

	t.Run("enabled accepts aggregate and both", func(t *testing.T) {
		svc, _, assignments, _ := newConfigProbeSvc(t)
		svc.SetAggregatePagingEnabled(true)
		if _, err := svc.Apply(ctx, 1, doc("aggregate"), services.ConfigApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Apply(ctx, 1, doc("both"), services.ConfigApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := onlySet(t, assignments).AlertDelivery; got != domain.AlertDeliveryBoth {
			t.Fatalf("enabled hub stored %q", got)
		}
	})
}

func onlySet(t *testing.T, repo *cfgAssignmentRepo) domain.MonitorProbeAssignments {
	t.Helper()
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.sets) != 1 {
		t.Fatalf("want one assignment set, have %d", len(repo.sets))
	}
	for _, set := range repo.sets {
		return *set
	}
	panic("unreachable")
}
