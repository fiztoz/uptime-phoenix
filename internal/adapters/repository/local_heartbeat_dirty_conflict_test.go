package repository_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TestLocalHeartbeatDirtyBucketConflictIsRetryable pins #162. Every local
// heartbeat marks its history buckets in probe_dirty_buckets inside the
// commit transaction, with or without remote probes, because local history
// feeds the overall uptime projection. The history projector concurrently
// rewrites and deletes those same rows. With innodb_snapshot_isolation (on by
// default since MariaDB 11.6) the commit's upsert of a row that changed after
// its read view fails with Error 1020 "Record has changed since last read".
// The engine aborts the whole transaction, exactly like 1213, so it must
// surface as ports.ErrStaleLocalState for HeartbeatService to retry instead
// of the scheduler dropping the heartbeat. MariaDB only.
func TestLocalHeartbeatDirtyBucketConflictIsRetryable(t *testing.T) {
	f := newProbeRegistryFixture(t, "mariadb")
	ctx := context.Background()
	id := localMonitor(t, f)
	trigger := "CREATE TRIGGER snapshot_conflict_dirty BEFORE INSERT ON probe_dirty_buckets FOR EACH ROW " +
		"SIGNAL SQLSTATE 'HY000' SET MYSQL_ERRNO = 1020, MESSAGE_TEXT = 'Record has changed since last read in table probe_dirty_buckets'"
	if _, err := f.db.ExecContext(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.db.ExecContext(ctx, "DROP TRIGGER IF EXISTS snapshot_conflict_dirty") })
	hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, localCommit(id, 0))
	if hb != nil {
		t.Fatalf("conflicted commit returned a heartbeat: %+v", hb)
	}
	if !errors.Is(err, ports.ErrStaleLocalState) {
		t.Fatalf("1020 on probe_dirty_buckets: want ErrStaleLocalState so the service retries, got %v", err)
	}
	if got := localSequence(t, f); got != 0 {
		t.Fatalf("conflicted commit consumed sequence %d", got)
	}
	var rows int
	if err := f.db.NewSelect().TableExpr("heartbeats").ColumnExpr("COUNT(*)").Where("monitor_id = ?", id).Scan(ctx, &rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("conflicted commit left %d heartbeat rows", rows)
	}
}

// TestLocalHeartbeatSurvivesConcurrentDirtyBucketWriters reproduces the soak
// interleaving without injection: a writer keeps committing changes to the
// monitor's own dirty-bucket rows (as the history projector does) while local
// heartbeats commit. Every commit must either succeed or report retryable
// contention; a raw engine error would be a dropped heartbeat.
func TestLocalHeartbeatSurvivesConcurrentDirtyBucketWriters(t *testing.T) {
	f := newProbeRegistryFixture(t, "mariadb")
	ctx := context.Background()
	id := localMonitor(t, f)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var writes atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := f.db.ExecContext(ctx, "UPDATE probe_dirty_buckets SET revision = UUID() WHERE monitor_id = ?", id); err == nil {
				writes.Add(1)
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	committed, retried := 0, 0
	deadline := time.Now().Add(20 * time.Second)
	for committed < 150 && time.Now().Before(deadline) {
		commit := localCommit(id, localSequence(t, f))
		commit.Heartbeat.Time = commit.Heartbeat.Time.Add(time.Duration(committed) * time.Second)
		commit.Heartbeat.ReceivedAt = commit.Heartbeat.Time
		_, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, commit)
		switch {
		case err == nil:
			committed++
		case errors.Is(err, ports.ErrStaleLocalState):
			retried++
		default:
			t.Fatalf("commit %d dropped by a non-retryable error after %d retries: %v", committed, retried, err)
		}
	}
	if committed == 0 || writes.Load() == 0 {
		t.Fatalf("no contention exercised: committed=%d writes=%d", committed, writes.Load())
	}
	t.Logf("committed=%d retried=%d concurrent dirty writes=%d", committed, retried, writes.Load())
}
