package services

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type standbyRepoFake struct {
	claims                     []domain.AggregateStandbyLease
	claimErr, errorOnReconcile error
	seen                       []domain.AggregateStandbyLease
	owner                      string
	ttl                        time.Duration
	limit                      int
}

func (f *standbyRepoFake) ClaimStandby(ctx context.Context, owner string, ttl time.Duration, limit int) ([]domain.AggregateStandbyLease, error) {
	f.owner = owner
	f.ttl = ttl
	f.limit = limit
	return f.claims, f.claimErr
}
func (f *standbyRepoFake) ReconcileStandby(ctx context.Context, l domain.AggregateStandbyLease, e ports.AggregateStandbyEvaluator) (domain.AggregateStandbyResult, error) {
	f.seen = append(f.seen, l)
	return domain.AggregateStandbyResult{}, f.errorOnReconcile
}
func TestAggregateStandbyService(t *testing.T) {
	leases := []domain.AggregateStandbyLease{{MonitorID: 7, OwnerID: "owner", Epoch: 1, Until: time.Now().UTC().Add(time.Minute)}, {MonitorID: 8, OwnerID: "owner", Epoch: 2, Until: time.Now().UTC().Add(time.Minute)}}
	f := &standbyRepoFake{claims: leases}
	s := NewAggregateStandbyService(f, "owner")
	before := append([]domain.AggregateStandbyLease(nil), leases...)
	if n, err := s.ProcessBatch(t.Context()); n != 2 || err != nil || !reflect.DeepEqual(f.seen, leases) || !reflect.DeepEqual(before, leases) || f.ttl != 30*time.Second || f.limit != 20 || f.owner != "owner" {
		t.Fatal("claim binding or borrowed input changed", n, err)
	}
	f.seen = nil
	f.errorOnReconcile = ports.ErrConflict
	if n, err := s.ProcessBatch(t.Context()); n != 0 || !errors.Is(err, ports.ErrConflict) || len(f.seen) != 2 {
		t.Fatal("failed reconciliation fabricated progress", n, err)
	}
	f.claimErr = domain.ErrInternal
	f.claims = nil
	f.seen = nil
	if n, err := s.ProcessBatch(t.Context()); n != 0 || !errors.Is(err, domain.ErrInternal) || len(f.seen) != 0 {
		t.Fatal("claim error ignored")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.ProcessBatch(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled context not preserved")
	}
	if _, err := NewAggregateStandbyService(nil, "").ProcessBatch(t.Context()); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("missing dependency disguised as success")
	}
	in := domain.AggregatePagingInput{Now: time.Now().UTC(), Current: true, Policy: domain.HealthPolicyAnyDown, DeliveryMode: domain.AlertDeliveryBoth, AssignmentRevision: 2, PolicyEffectiveAt: time.Now().UTC().Add(-time.Minute)}
	in.SnapshotAt = in.Now
	in.Evidence = []domain.RegionalHealthEvidence{{ProbeID: "local", Status: domain.StatusDown, ObservedAt: in.Now.Add(-time.Second), FreshFor: time.Minute}}
	got, err := s.EvaluateStandby(in)
	if err != nil || got.Action != domain.AggregatePagingOpen {
		t.Fatal("pure proposal not reused", err)
	}
}

type standbyJoinRepo struct {
	entered, release chan struct{}
	calls            atomic.Int64
}

func (f *standbyJoinRepo) ClaimStandby(ctx context.Context, owner string, ttl time.Duration, limit int) ([]domain.AggregateStandbyLease, error) {
	f.calls.Add(1)
	close(f.entered)
	<-f.release
	return nil, ctx.Err()
}
func (f *standbyJoinRepo) ReconcileStandby(context.Context, domain.AggregateStandbyLease, ports.AggregateStandbyEvaluator) (domain.AggregateStandbyResult, error) {
	return domain.AggregateStandbyResult{}, nil
}
func TestAggregateStandbyJoinedCancellation(t *testing.T) {
	f := &standbyJoinRepo{entered: make(chan struct{}), release: make(chan struct{})}
	s := NewAggregateStandbyService(f, "owner")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var reports atomic.Int64
	go func() { defer close(done); s.Run(ctx, time.Millisecond, func(error) { reports.Add(1) }) }()
	<-f.entered
	cancel()
	select {
	case <-done:
		t.Fatal("active repository work not joined")
	default:
	}
	close(f.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not join")
	}
	if f.calls.Load() != 1 || reports.Load() != 0 {
		t.Fatal("cancellation restarted or reported failed work")
	}
}
