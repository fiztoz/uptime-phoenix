package repository_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type historyReplayLockHook struct {
	after     func(context.Context, *bun.QueryEvent)
	deadlocks atomic.Int64
}

func (*historyReplayLockHook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}
func (h *historyReplayLockHook) AfterQuery(ctx context.Context, e *bun.QueryEvent) {
	var err *mysql.MySQLError
	if errors.As(e.Err, &err) && err.Number == 1213 {
		h.deadlocks.Add(1)
	}
	if h.after != nil {
		h.after(ctx, e)
	}
}

func TestHistoryCarryForwardUsesReplayLockOrder(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Minute).Add(-2*time.Minute + 55*time.Second)
	r := newReplayFixtureAt(t, "mariadb", at)
	r.ingest(t, r.batch(r.observation(1)))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	historyDB := reopenConfigDB(t, r.f)
	observer := reopenConfigDB(t, r.f)
	r.f.db.SetMaxOpenConns(1)
	var replayConnection int64
	if err := r.f.db.NewRaw("SELECT CONNECTION_ID()").Scan(ctx, &replayConnection); err != nil {
		t.Fatal(err)
	}
	locked, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	historyHook := &historyReplayLockHook{after: func(ctx context.Context, e *bun.QueryEvent) {
		if e.Err == nil && strings.HasPrefix(e.Query, "SELECT ") && strings.Contains(e.Query, "probe_dirty_buckets") && strings.HasSuffix(e.Query, "FOR UPDATE") {
			once.Do(func() {
				close(locked)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}
	}}
	replayHook := &historyReplayLockHook{}
	attachInPlaceQueryHook(historyDB, historyHook)
	attachInPlaceQueryHook(r.f.db, replayHook)
	type historyOutcome struct {
		processed int
		err       error
	}
	historyDone := make(chan historyOutcome, 1)
	go func() {
		n, e := services.NewProbeHistoryService(repository.NewRegionalCommitStore(historyDB)).ProcessBatch(ctx, time.Now().UTC(), 1)
		historyDone <- historyOutcome{n, e}
	}()
	select {
	case <-locked:
	case x := <-historyDone:
		t.Fatalf("history did not reach commit lock: %+v", x)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	type replayOutcome struct {
		result *domain.ProbeReplayResult
		err    error
	}
	replayDone := make(chan replayOutcome, 1)
	batch := r.batch(r.observation(2))
	go func() {
		v, e := r.store.IngestReplayBatch(ctx, r.session, batch, &services.AccessService{})
		replayDone <- replayOutcome{v, e}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var waitingQuery string
	for {
		var typ, name, status string
		if err := observer.QueryRowContext(ctx, "SHOW ENGINE INNODB STATUS").Scan(&typ, &name, &status); err != nil {
			t.Fatal(err)
		}
		for _, block := range strings.Split(status, "---TRANSACTION") {
			if strings.Contains(block, fmt.Sprintf("MariaDB thread id %d,", replayConnection)) && strings.Contains(block, "LOCK WAIT") {
				for _, line := range strings.Split(block, "\n") {
					if strings.HasPrefix(line, "UPDATE ") || strings.HasPrefix(line, "INSERT ") {
						waitingQuery = line
						break
					}
				}
			}
		}
		if waitingQuery != "" {
			break
		}
		select {
		case x := <-replayDone:
			t.Fatalf("replay escaped held history commit: %+v", x)
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("replay did not wait", ctx.Err())
		}
	}
	unblock()
	projected, ingested := <-historyDone, <-replayDone
	t.Logf("history_processed=%d history_err=%v replay_err=%v history_1213=%d replay_1213=%d replay_wait=%s", projected.processed, projected.err, ingested.err, historyHook.deadlocks.Load(), replayHook.deadlocks.Load(), waitingQuery)
	if projected.err != nil || projected.processed != 1 || historyHook.deadlocks.Load() != 0 || replayHook.deadlocks.Load() != 0 {
		t.Fatalf("history commit restarted behind replay: %+v", projected)
	}
	if ingested.err != nil || ingested.result.CommittedSeq != 2 || ingested.result.AcceptedCount != 1 {
		t.Fatalf("replay did not commit: %+v", ingested)
	}
	minute := readHistoryAggregate(t, r, "1m", at.Truncate(time.Minute))
	if minute.TotalChecks != 1 || !minute.HistoryManaged {
		t.Fatalf("history lost first projection: %+v", minute)
	}
	// Replay must re-mark the consumed source bucket after history commits, and
	// carry-forward must still queue the following minute in the same transaction.
	for _, bucket := range []time.Time{at.Truncate(time.Minute), at.Truncate(time.Minute).Add(time.Minute)} {
		n, err := r.f.db.NewSelect().Table("probe_dirty_buckets").Where("monitor_id=? AND probe_id=? AND resolution='1m' AND bucket=?", r.monitor, r.session.ProbeID, bucket).Count(ctx)
		if err != nil || n != 1 {
			t.Fatalf("durable re-mark/carry-forward missing at %s: %d %v", bucket, n, err)
		}
	}
	drainHistory(t, r)
	minute = readHistoryAggregate(t, r, "1m", at.Truncate(time.Minute))
	if minute.TotalChecks != 2 {
		t.Fatalf("recomputed history missed replay: %+v", minute)
	}
	again := r.ingest(t, batch)
	if again.DuplicateCount != 1 || again.AcceptedCount != 0 {
		t.Fatalf("replay duplicate changed source: %+v", again)
	}
}
