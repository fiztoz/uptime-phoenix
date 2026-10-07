package services

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func aggregatePagingInput() domain.AggregatePagingInput {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	return domain.AggregatePagingInput{
		Now: now, SnapshotAt: now, Current: true, Policy: domain.HealthPolicyAnyDown,
		DeliveryMode: domain.AlertDeliveryAggregate, AssignmentRevision: 2, PolicyEffectiveAt: now.Add(-time.Minute),
		Evidence: []domain.RegionalHealthEvidence{
			{ProbeID: "a", Status: domain.StatusDown, ObservedAt: now.Add(-time.Second), FreshFor: 90 * time.Second},
			{ProbeID: "b", Status: domain.StatusDown, ObservedAt: now.Add(-time.Second), FreshFor: 90 * time.Second},
		},
	}
}

func aggregatePagingIncident(input domain.AggregatePagingInput) *domain.AggregatePagingIncident {
	return &domain.AggregatePagingIncident{AssignmentRevision: input.AssignmentRevision, Policy: input.Policy, DeliveryMode: input.DeliveryMode}
}

func TestAggregatePagingTruthTable(t *testing.T) {
	up, down, pending, unknown, maintenance := domain.StatusUp, domain.StatusDown, domain.StatusPending, domain.StatusUnknown, domain.StatusMaintenance
	for _, tc := range []struct {
		name                    string
		first, second, any, all domain.Status
	}{
		{"up", up, up, up, up}, {"down", down, down, down, down},
		{"recovering region", down, up, down, up}, {"down unknown", down, unknown, down, unknown},
		{"down pending", down, pending, down, pending}, {"up unknown", up, unknown, unknown, up},
		{"up pending", up, pending, pending, up}, {"all unknown", unknown, unknown, unknown, unknown},
		{"all pending", pending, pending, pending, pending}, {"unknown pending", unknown, pending, unknown, unknown},
		{"all maintenance", maintenance, maintenance, maintenance, maintenance},
		{"maintenance down", maintenance, down, down, down}, {"maintenance unknown", maintenance, unknown, unknown, unknown},
	} {
		for _, policy := range []domain.HealthPolicy{domain.HealthPolicyAnyDown, domain.HealthPolicyAllDown} {
			for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth, domain.AlertDeliveryRegional, ""} {
				for _, open := range []bool{false, true} {
					if open && (mode == "" || mode == domain.AlertDeliveryRegional) {
						continue
					}
					t.Run(tc.name+"/"+string(policy)+"/"+string(mode)+"/open="+map[bool]string{true: "true", false: "false"}[open], func(t *testing.T) {
						input := aggregatePagingInput()
						input.Policy, input.DeliveryMode = policy, mode
						input.Evidence[0].Status, input.Evidence[1].Status = tc.first, tc.second
						if open {
							input.OpenIncident = aggregatePagingIncident(input)
						}
						want := tc.any
						if policy == domain.HealthPolicyAllDown {
							want = tc.all
						}
						action := domain.AggregatePagingHold
						if mode == domain.AlertDeliveryAggregate || mode == domain.AlertDeliveryBoth {
							if want == down && !open {
								action = domain.AggregatePagingOpen
							}
							if want == up && open {
								action = domain.AggregatePagingRecover
							}
						}
						got, err := EvaluateAggregatePaging(input)
						if err != nil || got.Health.Status != want || got.Action != action || got.AdministrativeReason != "" {
							t.Fatalf("got %+v %v; want %v %v", got, err, want, action)
						}
					})
				}
			}
		}
	}
}

func TestAggregatePagingIncompleteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		change   func(*domain.AggregatePagingInput)
		any, all domain.Status
	}{
		{"missing", func(i *domain.AggregatePagingInput) { i.Evidence[1].ObservedAt = time.Time{} }, domain.StatusDown, domain.StatusUnknown},
		{"stale", func(i *domain.AggregatePagingInput) {
			i.PolicyEffectiveAt = i.Now.Add(-2 * time.Minute)
			i.Evidence[1].ObservedAt = i.Now.Add(-90 * time.Second)
		}, domain.StatusDown, domain.StatusUnknown},
		{"future", func(i *domain.AggregatePagingInput) { i.Evidence[1].ObservedAt = i.Now.Add(time.Nanosecond) }, domain.StatusDown, domain.StatusUnknown},
		{"no freshness", func(i *domain.AggregatePagingInput) { i.Evidence[1].FreshFor = 0 }, domain.StatusDown, domain.StatusUnknown},
		{"gap", func(i *domain.AggregatePagingInput) { i.Evidence[1].UnknownReason = "retention_gap" }, domain.StatusDown, domain.StatusUnknown},
		{"paused member", func(i *domain.AggregatePagingInput) { i.Evidence[1].Paused = true }, domain.StatusDown, domain.StatusDown},
		{"all paused", func(i *domain.AggregatePagingInput) { i.Evidence[0].Paused, i.Evidence[1].Paused = true, true }, domain.StatusUnknown, domain.StatusUnknown},
		{"old policy", func(i *domain.AggregatePagingInput) {
			i.Evidence[1].ObservedAt = i.PolicyEffectiveAt.Add(-time.Nanosecond)
		}, domain.StatusDown, domain.StatusUnknown},
		{"boundary", func(i *domain.AggregatePagingInput) { i.Evidence[1].ObservedAt = i.PolicyEffectiveAt }, domain.StatusDown, domain.StatusUnknown},
	} {
		for _, policy := range []domain.HealthPolicy{domain.HealthPolicyAnyDown, domain.HealthPolicyAllDown} {
			t.Run(tc.name+"/"+string(policy), func(t *testing.T) {
				i := aggregatePagingInput()
				i.Policy = policy
				tc.change(&i)
				want := tc.any
				if policy == domain.HealthPolicyAllDown {
					want = tc.all
				}
				got, err := EvaluateAggregatePaging(i)
				if err != nil || got.Health.Status != want {
					t.Fatalf("got %+v %v want %v", got, err, want)
				}
				if want == domain.StatusDown && got.Action != domain.AggregatePagingOpen {
					t.Fatal(got)
				}
				i.OpenIncident = aggregatePagingIncident(i)
				got, err = EvaluateAggregatePaging(i)
				if err != nil || got.Action != domain.AggregatePagingHold {
					t.Fatalf("unknown/down must hold incident: %+v %v", got, err)
				}
			})
		}
	}
}

func TestAggregatePagingAdministrativeBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		change       func(*domain.AggregatePagingInput)
	}{
		{"membership", "assignment_revision_changed", func(i *domain.AggregatePagingInput) {}},
		{"policy", "health_policy_changed", func(i *domain.AggregatePagingInput) { i.Policy = domain.HealthPolicyAllDown }},
		{"mode both", "delivery_mode_changed", func(i *domain.AggregatePagingInput) { i.DeliveryMode = domain.AlertDeliveryBoth }},
		{"mode regional", "delivery_mode_changed", func(i *domain.AggregatePagingInput) { i.DeliveryMode = domain.AlertDeliveryRegional }},
		{"mode default", "delivery_mode_changed", func(i *domain.AggregatePagingInput) { i.DeliveryMode = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := aggregatePagingInput()
			i.OpenIncident = aggregatePagingIncident(i)
			i.AssignmentRevision++
			i.PolicyEffectiveAt = i.Now.Add(-2 * time.Second)
			tc.change(&i)
			got, err := EvaluateAggregatePaging(i)
			if err != nil || got.Health.Status != domain.StatusDown || got.Action != domain.AggregatePagingAdminClose {
				t.Fatalf("configuration change with fresh DOWN must only close: %+v %v", got, err)
			}
			i.PolicyEffectiveAt = i.Now
			got, err = EvaluateAggregatePaging(i)
			if err != nil || got.Action != domain.AggregatePagingAdminClose || got.AdministrativeReason != tc.reason {
				t.Fatalf("got %+v %v", got, err)
			}
			i.OpenIncident = nil
			got, err = EvaluateAggregatePaging(i)
			if err != nil || got.Action != domain.AggregatePagingHold {
				t.Fatalf("reused old evidence: %+v %v", got, err)
			}
			i.Now = i.Now.Add(time.Second)
			i.SnapshotAt = i.Now
			for n := range i.Evidence {
				i.Evidence[n].ObservedAt = i.Now
			}
			got, err = EvaluateAggregatePaging(i)
			want := domain.AggregatePagingOpen
			if i.DeliveryMode == "" || i.DeliveryMode == domain.AlertDeliveryRegional {
				want = domain.AggregatePagingHold
			}
			if err != nil || got.Action != want {
				t.Fatalf("fresh evidence: %+v %v", got, err)
			}
		})
	}
}

func TestAggregatePagingHistoricalReplayNeverTransitions(t *testing.T) {
	for _, status := range []domain.Status{domain.StatusDown, domain.StatusUp} {
		for _, open := range []bool{false, true} {
			for _, changed := range []bool{false, true} {
				i := aggregatePagingInput()
				i.Current = false
				i.Now = i.Now.Add(time.Minute)
				for n := range i.Evidence {
					i.Evidence[n].Status = status
				}
				if open {
					i.OpenIncident = aggregatePagingIncident(i)
				}
				if changed {
					i.AssignmentRevision++
					i.Policy = domain.HealthPolicyAllDown
				}
				got, err := EvaluateAggregatePaging(i)
				if err != nil || got.Action != domain.AggregatePagingHold || got.AdministrativeReason != "" {
					t.Fatalf("historical transition: %+v %v", got, err)
				}
			}
		}
	}
}

func TestAggregatePagingInvalidInputHolds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*domain.AggregatePagingInput)
	}{
		{"zero now", func(i *domain.AggregatePagingInput) { i.Now = time.Time{} }},
		{"zero snapshot", func(i *domain.AggregatePagingInput) { i.SnapshotAt = time.Time{} }},
		{"stale snapshot", func(i *domain.AggregatePagingInput) { i.SnapshotAt = i.Now.Add(-time.Nanosecond) }},
		{"future snapshot", func(i *domain.AggregatePagingInput) { i.SnapshotAt = i.Now.Add(time.Nanosecond) }},
		{"future boundary", func(i *domain.AggregatePagingInput) { i.PolicyEffectiveAt = i.Now.Add(time.Nanosecond) }},
		{"zero boundary", func(i *domain.AggregatePagingInput) { i.PolicyEffectiveAt = time.Time{} }},
		{"revision", func(i *domain.AggregatePagingInput) { i.AssignmentRevision = 0 }},
		{"negative revision", func(i *domain.AggregatePagingInput) { i.AssignmentRevision = -1 }},
		{"mode", func(i *domain.AggregatePagingInput) { i.DeliveryMode = "other" }},
		{"policy", func(i *domain.AggregatePagingInput) { i.Policy = "other" }},
		{"empty quorum", func(i *domain.AggregatePagingInput) { i.Evidence = nil }},
		{"duplicate", func(i *domain.AggregatePagingInput) { i.Evidence[1].ProbeID = i.Evidence[0].ProbeID }},
		{"empty identity", func(i *domain.AggregatePagingInput) { i.Evidence[0].ProbeID = "" }},
		{"invalid status", func(i *domain.AggregatePagingInput) { i.Evidence[0].Status = 99 }},
		{"negative freshness", func(i *domain.AggregatePagingInput) { i.Evidence[0].FreshFor = -1 }},
		{"old snapshot revision", func(i *domain.AggregatePagingInput) {
			i.OpenIncident = aggregatePagingIncident(*i)
			i.OpenIncident.AssignmentRevision++
		}},
		{"invalid incident revision", func(i *domain.AggregatePagingInput) {
			i.OpenIncident = aggregatePagingIncident(*i)
			i.OpenIncident.AssignmentRevision = 0
		}},
		{"invalid incident policy", func(i *domain.AggregatePagingInput) {
			i.OpenIncident = aggregatePagingIncident(*i)
			i.OpenIncident.Policy = "other"
		}},
		{"invalid incident mode", func(i *domain.AggregatePagingInput) {
			i.OpenIncident = aggregatePagingIncident(*i)
			i.OpenIncident.DeliveryMode = "other"
		}},
		{"regional incident", func(i *domain.AggregatePagingInput) {
			i.OpenIncident = aggregatePagingIncident(*i)
			i.OpenIncident.DeliveryMode = domain.AlertDeliveryRegional
		}},
		{"same revision policy change", func(i *domain.AggregatePagingInput) {
			i.OpenIncident = aggregatePagingIncident(*i)
			i.Policy = domain.HealthPolicyAllDown
		}},
		{"same revision mode change", func(i *domain.AggregatePagingInput) {
			i.OpenIncident = aggregatePagingIncident(*i)
			i.DeliveryMode = domain.AlertDeliveryBoth
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := aggregatePagingInput()
			tc.change(&i)
			got, err := EvaluateAggregatePaging(i)
			if !errors.Is(err, ErrInvalidAggregatePaging) || got.Action != domain.AggregatePagingHold {
				t.Fatalf("got %+v %v", got, err)
			}
		})
	}
}

func TestAggregatePagingDoesNotMutateInputAndAcceptsLargeRevision(t *testing.T) {
	i := aggregatePagingInput()
	i.AssignmentRevision = math.MaxInt64
	i.OpenIncident = aggregatePagingIncident(i)
	i.Evidence[0].ObservedAt = i.PolicyEffectiveAt
	i.Now = i.Now.In(time.FixedZone("UTC+7", 7*3600))
	before := i
	before.Evidence = append([]domain.RegionalHealthEvidence(nil), i.Evidence...)
	incident := *i.OpenIncident
	before.OpenIncident = &incident
	got, err := EvaluateAggregatePaging(i)
	if err != nil || got.Action != domain.AggregatePagingHold {
		t.Fatalf("got %+v %v", got, err)
	}
	if !reflect.DeepEqual(i, before) {
		t.Fatal("mutated input")
	}
}
