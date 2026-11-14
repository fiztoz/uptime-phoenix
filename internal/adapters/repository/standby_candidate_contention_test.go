package repository_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// standbyCandidateBarrier observes successful selector completion, while holding
// the caller before its authority transaction. It never changes a driver result.
type standbyCandidateBarrier struct {
	once     sync.Once
	selected chan struct{}
	release  chan struct{}
}

func (h *standbyCandidateBarrier) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}
func (h *standbyCandidateBarrier) AfterQuery(ctx context.Context, e *bun.QueryEvent) {
	if e.Err != nil || !strings.Contains(e.Query, "LEFT JOIN aggregate_standby_state") {
		return
	}
	h.once.Do(func() {
		close(h.selected)
		select {
		case <-h.release:
		case <-ctx.Done():
		}
	})
}

// Candidate discovery is not authority. It must not acquire shared predicate
// locks that conflict with an unrelated assignment writer, and a stale candidate
// must still be rejected by the subsequent authoritative eligibility check.
func TestStandbyCandidateSnapshotDoesNotLockAndRechecksEligibility(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		name := "still_eligible"
		if revoke {
			name = "eligibility_revoked"
		}
		t.Run(name, func(t *testing.T) {
			s := newStandbyFixture(t, "mariadb", domain.AlertDeliveryAggregate)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if _, err := s.p.f.db.ExecContext(ctx, "UPDATE aggregate_standby_state SET lease_until=?,due_at=? WHERE monitor_id=?", time.Now().UTC().Add(-time.Minute), time.Now().UTC().Add(-time.Minute), s.p.monitor.ID); err != nil {
				t.Fatal(err)
			}
			writer := reopenConfigDB(t, s.p.f)
			tx, err := writer.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }() // release the owned lock even when the regression fails
			mode := domain.AlertDeliveryAggregate
			if revoke {
				mode = domain.AlertDeliveryRegional
			}
			if _, err := tx.ExecContext(ctx, "UPDATE monitor_probe_assignment_sets SET alert_delivery=? WHERE monitor_id=?", mode, s.p.monitor.ID); err != nil {
				t.Fatal(err)
			}
			peer := reopenConfigDB(t, s.p.f)
			barrier := &standbyCandidateBarrier{selected: make(chan struct{}), release: make(chan struct{})}
			attachInPlaceQueryHook(peer, barrier)
			type outcome struct {
				leases []domain.AggregateStandbyLease
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				leases, err := repository.NewAggregateStandbyStore(peer).ClaimStandby(ctx, standbyOwner, 30*time.Second, 20)
				done <- outcome{leases, err}
			}()
			select {
			case <-barrier.selected:
			case <-ctx.Done():
				_ = tx.Rollback()
				<-done
				t.Fatal("candidate discovery waited on assignment writer; selector acquired authority locks")
			}
			if err := tx.Commit(); err != nil {
				close(barrier.release)
				<-done
				t.Fatal(err)
			}
			close(barrier.release)
			got := <-done
			if got.err != nil {
				t.Fatal(got.err)
			}
			want := 1
			if revoke {
				want = 0
			}
			if len(got.leases) != want {
				t.Fatalf("claim count %d; want %d after committed eligibility change", len(got.leases), want)
			}
		})
	}
}
