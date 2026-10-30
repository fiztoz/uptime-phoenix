package services

import (
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// TestAggregateAlertDeliveryRefusedWhilePagingDisabled pins #119. With
// aggregate paging off (the default), a hub never sends aggregate alerts and
// keeps regional paging, so accepting a request for aggregate or both would
// report a mode that does nothing. The write must be refused before any
// commit. A stored legacy mode may be resubmitted unchanged or omitted,
// because clients echo the current mode, and it may always move to regional.
func TestAggregateAlertDeliveryRefusedWhilePagingDisabled(t *testing.T) {
	monitor := &domain.Monitor{ID: 7, Type: "http"}
	setWith := func(mode domain.AlertDelivery) *domain.MonitorProbeAssignments {
		return &domain.MonitorProbeAssignments{MonitorID: 7, Revision: 4, HealthPolicy: domain.HealthPolicyAnyDown, AlertDelivery: mode,
			Assignments: []domain.ProbeAssignment{{MonitorID: 7, ProbeID: domain.LocalProbeID, Generation: 1}}}
	}
	for _, tc := range []struct {
		name      string
		stored    domain.AlertDelivery
		requested string
		reject    bool
	}{
		{"regional to aggregate", domain.AlertDeliveryRegional, "aggregate", true},
		{"regional to both", domain.AlertDeliveryRegional, "both", true},
		{"legacy empty to aggregate", "", "aggregate", true},
		{"stored aggregate to both", domain.AlertDeliveryAggregate, "both", true},
		{"stored aggregate resubmitted", domain.AlertDeliveryAggregate, "aggregate", false},
		{"stored aggregate omitted", domain.AlertDeliveryAggregate, "", false},
		{"stored aggregate to regional", domain.AlertDeliveryAggregate, "regional", false},
		{"regional", domain.AlertDeliveryRegional, "regional", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := setWith(tc.stored)
			writer := &fakeAssignmentWriter{result: setWith(tc.stored)}
			svc := NewProbeAssignmentService(writer, fakeAssignmentRepo{set: previous}, assignmentTestProbes(), fakeMonitorLookup{monitor: monitor}, assignmentTestCaps(), awareFleetGate())
			_, err := svc.Replace(t.Context(), 7, ProbeAssignmentRequest{ExpectedRevision: 4, ProbeIDs: []string{domain.LocalProbeID}, HealthPolicy: domain.HealthPolicyAnyDown, AlertDelivery: tc.requested})
			if tc.reject {
				if !errors.Is(err, ErrInvalidDelivery) || writer.replaced {
					t.Fatalf("alert_delivery %q accepted while aggregate paging is disabled (err %v, written %v)", tc.requested, err, writer.replaced)
				}
				return
			}
			if err != nil || !writer.replaced {
				t.Fatalf("alert_delivery %q refused: %v", tc.requested, err)
			}
		})
	}

	t.Run("create with aggregate", func(t *testing.T) {
		writer := &fakeAssignmentWriter{result: setWith(domain.AlertDeliveryAggregate)}
		svc := NewMonitorService(newCloneFakeMonitorRepo(), newFakeBus())
		svc.SetAssignmentProvisioning(writer, assignmentTestProbes(), assignmentTestCaps(), awareFleetGate())
		err := svc.CreateWithAssignments(t.Context(), &domain.Monitor{UserID: 1, Name: "m", Type: "http", Active: true, Interval: 60, Timeout: 5, Config: map[string]any{"url": "https://example.com"}}, InitialAssignments{ProbeIDs: []string{domain.LocalProbeID}, HealthPolicy: domain.HealthPolicyAnyDown, AlertDelivery: "both"})
		if !errors.Is(err, ErrInvalidDelivery) || writer.created {
			t.Fatalf("create with alert_delivery both accepted while aggregate paging is disabled (err %v, created %v)", err, writer.created)
		}
	})

	t.Run("paging enabled accepts aggregate and both", func(t *testing.T) {
		for _, mode := range []string{"aggregate", "both"} {
			writer := &fakeAssignmentWriter{result: setWith(domain.AlertDelivery(mode))}
			svc := NewProbeAssignmentService(writer, fakeAssignmentRepo{set: setWith(domain.AlertDeliveryRegional)}, assignmentTestProbes(), fakeMonitorLookup{monitor: monitor}, assignmentTestCaps(), awareFleetGate())
			svc.SetAggregatePagingEnabled(true)
			if _, err := svc.Replace(t.Context(), 7, ProbeAssignmentRequest{ExpectedRevision: 4, ProbeIDs: []string{domain.LocalProbeID}, HealthPolicy: domain.HealthPolicyAnyDown, AlertDelivery: mode}); err != nil || writer.delivery != domain.AlertDelivery(mode) {
				t.Fatalf("%s refused with paging enabled: %v (stored %q)", mode, err, writer.delivery)
			}
		}
	})

	t.Run("refusal names the disabled flag", func(t *testing.T) {
		if err := requireAggregatePaging(domain.AlertDeliveryBoth, nil, false); !errors.Is(err, ErrAggregatePagingDisabled) {
			t.Fatalf("got %v, want ErrAggregatePagingDisabled", err)
		}
	})
}
