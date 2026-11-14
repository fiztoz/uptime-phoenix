package repository_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// operativeCandidateBarrier observes successful selector completion, while holding
// the caller before its authority transaction. It never changes a driver result.
type operativeCandidateBarrier struct {
	once     sync.Once
	selected chan struct{}
	release  chan struct{}
	query    string
}

func (h *operativeCandidateBarrier) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}
func (h *operativeCandidateBarrier) AfterQuery(ctx context.Context, e *bun.QueryEvent) {
	if e.Err != nil || !strings.Contains(e.Query, h.query) {
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
func TestOperativeCandidateSnapshotDoesNotLockAndRechecksEligibility(t *testing.T) {
	for _, name := range []string{"still_eligible", "eligibility_revoked", "orphan_draining"} {
		revoke := name == "eligibility_revoked"
		orphan := name == "orphan_draining"
		t.Run(name, func(t *testing.T) {
			s := newOperativeFixture(t, "mariadb", domain.AlertDeliveryAggregate, 1, orphan)
			if orphan {
				if got := s.run(t, true); got.Phase != "quiescing" {
					t.Fatalf("prepare barrier: %+v", got)
				}
				if got := s.run(t, false); got.Phase != "draining" {
					t.Fatalf("drain barrier: %+v", got)
				}
				if _, err := s.p.f.db.ExecContext(t.Context(), "DELETE FROM monitor_probe_assignment_sets WHERE monitor_id=?", s.p.monitor.ID); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if _, err := s.p.f.db.ExecContext(ctx, "UPDATE operative_cutover_state SET lease_until=?,due_at=? WHERE monitor_id=?", time.Now().UTC().Add(-time.Minute), time.Now().UTC().Add(-time.Minute), s.p.monitor.ID); err != nil {
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
			writerSQL := "UPDATE monitor_probe_assignment_sets SET alert_delivery=? WHERE monitor_id=?"
			query := "LEFT JOIN operative_cutover_state"
			args := []any{mode, s.p.monitor.ID}
			if orphan {
				writerSQL = "UPDATE operative_cutover_state SET due_at=due_at WHERE monitor_id=?"
				args = []any{s.p.monitor.ID}
				query = "monitor_id NOT IN (SELECT monitor_id FROM monitor_probe_assignment_sets)"
			}
			if _, err := tx.ExecContext(ctx, writerSQL, args...); err != nil {
				t.Fatal(err)
			}
			peer := reopenConfigDB(t, s.p.f)
			peerFixture := s.p.f
			peerFixture.db = peer
			barrier := &operativeCandidateBarrier{selected: make(chan struct{}), release: make(chan struct{}), query: query}
			attachInPlaceQueryHook(peer, barrier)
			type outcome struct {
				leases []domain.AggregateStandbyLease
				err    error
			}
			done := make(chan outcome, 1)
			store := newOperativeStore(t, peerFixture)
			go func() {
				leases, err := store.ClaimCutovers(ctx, operativeOwner, 30*time.Second, 20)
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
