package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type localSnapshotWindowHook struct {
	match  func(string) bool
	mutate func()
	fired  bool
}

func (h *localSnapshotWindowHook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}
func (h *localSnapshotWindowHook) AfterQuery(_ context.Context, e *bun.QueryEvent) {
	if !h.fired && (e.Err == nil || errors.Is(e.Err, sql.ErrNoRows)) && h.match(e.Query) {
		h.fired = true
		h.mutate()
	}
}

// A native writer changes the exact dirty rows after a selected read, without
// injecting a driver error. This pins where the transaction creates its RR view.
func TestLocalHeartbeatSnapshotWindow(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "AllocatorRead"
		if late {
			name = "HistoryRead"
		}
		t.Run(name, func(t *testing.T) {
			f := newProbeRegistryFixture(t, "mariadb")
			ctx := context.Background()
			id := localMonitor(t, f)
			first, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, localCommit(id, 0))
			if err != nil {
				t.Fatal(err)
			}
			var mutationErr error
			h := &localSnapshotWindowHook{match: func(q string) bool {
				if !strings.HasPrefix(q, "SELECT") {
					return false
				}
				if late {
					return strings.Contains(q, "probe_observations") && strings.Contains(q, "LIMIT 1")
				}
				return strings.Contains(q, "probe_local_sequence") && strings.Contains(q, "last_seq")
			}, mutate: func() {
				_, mutationErr = f.db.ExecContext(ctx, "UPDATE probe_dirty_buckets SET revision = UUID() WHERE monitor_id = ?", id)
			}}
			attachInPlaceQueryHook(f.db, h)
			c := localCommit(id, first.SourceSeq)
			c.Heartbeat.Time = c.Heartbeat.Time.Add(time.Second)
			c.Heartbeat.ReceivedAt = c.Heartbeat.Time
			hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, c)
			if !h.fired || mutationErr != nil {
				t.Fatalf("native mutation not exercised: fired=%v err=%v", h.fired, mutationErr)
			}
			if late {
				if hb != nil || !errors.Is(err, ports.ErrStaleLocalState) {
					t.Fatalf("late snapshot change must roll back retryably: hb=%v err=%v", hb, err)
				}
				if got := localSequence(t, f); got != first.SourceSeq {
					t.Fatalf("rollback consumed sequence: %d", got)
				}
			} else {
				if err != nil {
					t.Fatalf("allocator read must not establish a stale snapshot before authority locks: %v", err)
				}
				if hb.SourceSeq != first.SourceSeq+1 {
					t.Fatalf("sequence=%d", hb.SourceSeq)
				}
			}
		})
	}
}
