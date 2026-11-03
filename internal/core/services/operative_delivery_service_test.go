package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type cutoverBatchFake struct {
	leases []domain.AggregateStandbyLease
	claim  error
	fail   map[int64]error
}

func (f *cutoverBatchFake) ClaimCutovers(context.Context, string, time.Duration, int) ([]domain.AggregateStandbyLease, error) {
	return f.leases, f.claim
}

func (f *cutoverBatchFake) ReconcileCutover(_ context.Context, lease domain.AggregateStandbyLease, _ bool, _ ports.AggregateCutoverEvaluator) (domain.AggregateCutoverProgress, error) {
	return domain.AggregateCutoverProgress{MonitorID: lease.MonitorID}, f.fail[lease.MonitorID]
}

type idleAggregateDeliveries struct{}

func (idleAggregateDeliveries) ClaimAggregateDelivery(context.Context, string, time.Duration) (*domain.QueuedDelivery, error) {
	return nil, ports.ErrNotFound
}

func (idleAggregateDeliveries) AuthorizeAggregateDelivery(context.Context, domain.DeliveryClaim, time.Duration) (*domain.AggregateProviderGrant, error) {
	return nil, ports.ErrNotFound
}

func (idleAggregateDeliveries) FinishAggregateDelivery(context.Context, domain.DeliveryClaim, domain.DeliveryResult) error {
	return nil
}

type idleAttemptJournal struct{}

func (idleAttemptJournal) FinishSourceProviderAttempt(context.Context, string, domain.ProviderAttemptState) error {
	return nil
}

// TestOperativeReconcileBatchReportsStageAndMonitor proves a reconciliation
// failure reaches the operator report with its stage and monitor attached
// (issue #191: the composition root used to log a detail-free warning), keeps
// the typed sentinel for errors.Is, and never discards another monitor's work.
func TestOperativeReconcileBatchReportsStageAndMonitor(t *testing.T) {
	fake := &cutoverBatchFake{
		leases: []domain.AggregateStandbyLease{{MonitorID: 13}, {MonitorID: 14}},
		claim:  domain.ErrInternal,
		fail:   map[int64]error{13: ports.ErrConflict},
	}
	svc, err := NewOperativeDeliveryService(fake, idleAggregateDeliveries{}, idleAttemptJournal{},
		"99999999-9999-4999-8999-999999999999", true, func(string) (ports.NotificationSender, bool) { return nil, false })
	if err != nil {
		t.Fatal(err)
	}
	n, err := svc.ReconcileBatch(t.Context())
	if n != 1 {
		t.Fatalf("reconciled %d monitors, want the healthy one", n)
	}
	if !errors.Is(err, ports.ErrConflict) || !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("typed sentinels lost: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"operative cutover reconciliation (monitor 13)", "operative cutover claim"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("report %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "monitor 14") {
		t.Fatalf("healthy monitor reported as failed: %q", msg)
	}
}
