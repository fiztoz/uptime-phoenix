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

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type replayAssignmentLockHook struct {
	after          func(context.Context, *bun.QueryEvent)
	deadlocks      atomic.Int64
	legacyAttempts atomic.Int64
}

func (h *replayAssignmentLockHook) BeforeQuery(ctx context.Context, e *bun.QueryEvent) context.Context {
	if strings.Contains(e.Query, "NOT EXISTS (SELECT 1 FROM monitor_probe_assignment_sets") {
		h.legacyAttempts.Add(1)
	}
	return ctx
}
func (h *replayAssignmentLockHook) AfterQuery(ctx context.Context, e *bun.QueryEvent) {
	var err *mysql.MySQLError
	if errors.As(e.Err, &err) && err.Number == 1213 {
		h.deadlocks.Add(1)
	}
	if h.after != nil {
		h.after(ctx, e)
	}
}
func TestObservationReplayLockOrderWithAppliedSourceReader(t *testing.T) {
	for _, descending := range []bool{true, false} {
		t.Run(fmt.Sprintf("descending_%v", descending), func(t *testing.T) {
			r := newReplayFixture(t, "mariadb")
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			high := r.f.monitor(t)
			if _, err := r.f.assignments.InitializeLocal(ctx, high); err != nil {
				t.Fatal(err)
			}
			if _, err := r.f.assignments.Replace(ctx, high, 1, []string{r.session.ProbeID}, domain.HealthPolicyAnyDown); err != nil {
				t.Fatal(err)
			}
			if _, err := r.f.db.ExecContext(ctx, "UPDATE monitor_probe_assignment_history SET started_at = ?", r.at.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			meta, err := r.syncer.RefreshRemote(ctx, syncTarget(), r.at.Add(-time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			builder, _ := sourceConfigBuilder(t, r.f, r.f.db)
			local, err := builder.Prepare(ctx, r.session.HubID, 0, r.at, r.at)
			if err != nil {
				t.Fatal(err)
			}
			readerDB := reopenConfigDB(t, r.f)
			readerDB.SetMaxOpenConns(1)
			reader := repository.NewProbeActivationStore(readerDB, probe.LocalConfigEncoder{})
			if _, err := reader.ActivateLocal(ctx, ports.LocalActivationParams{Target: local.ProbeConfigTarget, Revision: local.Revision, SHA256: local.SHA256, AppliedAt: r.at, AssignmentCount: 0}); err != nil {
				t.Fatal(err)
			}
			var connection int64
			if err := readerDB.NewRaw("SELECT CONNECTION_ID()").Scan(ctx, &connection); err != nil {
				t.Fatal(err)
			}
			observer := reopenConfigDB(t, r.f)
			first, second := r.monitor, high
			if descending {
				first, second = high, r.monitor
			}
			e1, e2 := r.observation(1), r.observation(2)
			e1.Observation.MonitorID = first
			e2.Observation.MonitorID = second
			e1.Observation.ConfigRevision = meta.Revision
			e2.Observation.ConfigRevision = meta.Revision
			batch := r.batch(e1, e2)
			reached, release := make(chan struct{}), make(chan struct{})
			var once, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			writerHook := &replayAssignmentLockHook{after: func(ctx context.Context, e *bun.QueryEvent) {
				if e.Err == nil && e.Query == fmt.Sprintf("UPDATE monitor_probe_assignment_sets SET revision = revision WHERE monitor_id = %d", first) {
					once.Do(func() {
						close(reached)
						select {
						case <-release:
						case <-ctx.Done():
						}
					})
				}
			}}
			readerHook := &replayAssignmentLockHook{}
			attachInPlaceQueryHook(r.f.db, writerHook)
			attachInPlaceQueryHook(readerDB, readerHook)
			type outcome struct {
				result *domain.ProbeReplayResult
				err    error
			}
			replayDone := make(chan outcome, 1)
			readDone := make(chan error, 1)
			go func() {
				v, e := r.store.IngestReplayBatch(ctx, r.session, batch, &services.AccessService{})
				replayDone <- outcome{v, e}
			}()
			select {
			case <-reached:
			case <-ctx.Done():
				t.Fatal("replay did not hold first assignment", ctx.Err())
			}
			go func() {
				v, e := reader.ReadAppliedLocal(ctx)
				if e == nil && len(v.Assignments) != 0 {
					e = fmt.Errorf("remote-only source acquired local assignments")
				}
				readDone <- e
			}()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			var lockData string
			for {
				// MariaDB omits this read-only trx id 0 from INNODB_LOCK_WAITS.
				// Observe the actual wait in InnoDB status instead of relying on a sleep.
				var status struct {
					Type   string
					Name   string
					Status string
				}
				if err := observer.QueryRowContext(ctx, "SHOW ENGINE INNODB STATUS").Scan(&status.Type, &status.Name, &status.Status); err != nil {
					t.Fatal(err)
				}
				waiting := false
				for _, block := range strings.Split(status.Status, "---TRANSACTION") {
					if strings.Contains(block, fmt.Sprintf("MariaDB thread id %d,", connection)) && strings.Contains(block, "LOCK WAIT") && strings.Contains(block, "monitor_probe_assignment_sets") {
						waiting = true
						lockData = "shared assignment lock; read-only trx id 0"
						break
					}
				}
				if waiting {
					break
				}
				select {
				case e := <-readDone:
					t.Fatalf("reader returned before wait: %v, legacy_attempts=%d", e, readerHook.legacyAttempts.Load())
				case <-ticker.C:
				case <-ctx.Done():
					t.Fatalf("reader did not wait: %v legacy_attempts=%d", ctx.Err(), readerHook.legacyAttempts.Load())
				}
			}
			unblock()
			if err := <-readDone; err != nil {
				t.Fatal("reader failed after retry", err)
			}
			got := <-replayDone
			if got.err != nil {
				t.Fatal("replay failed", got.err)
			}
			if got.result.CommittedSeq != 2 || got.result.AcceptedCount != 2 || len(got.result.Rejected) != 0 {
				t.Fatalf("bad receipt: %+v", got.result)
			}
			var recorded []int64
			if err := r.f.db.NewSelect().Table("probe_observations").Column("monitor_id").Order("seq ASC").Scan(ctx, &recorded); err != nil {
				t.Fatal(err)
			}
			if len(recorded) != 2 || recorded[0] != first || recorded[1] != second {
				t.Fatalf("source event order changed: %v", recorded)
			}
			deadlocks := readerHook.deadlocks.Load() + writerHook.deadlocks.Load()
			if deadlocks != 0 {
				t.Fatalf("observation replay deadlocked applied source reader: deadlocks=%d", deadlocks)
			}
			if n := replayCount(t, r.f, "probe_telemetry_receipts"); n != 2 {
				t.Fatalf("receipts=%d", n)
			}
			again := r.ingest(t, batch)
			if again.DuplicateCount != 2 || again.AcceptedCount != 0 {
				t.Fatalf("retry duplicated evidence: %+v", again)
			}
			t.Logf("first_monitor=%d second_monitor=%d reader_wait_lock=%s reader_1213=%d replay_1213=%d legacy_attempts=%d cursor=%d accepted=%d duplicate_retry=%d", first, second, lockData, readerHook.deadlocks.Load(), writerHook.deadlocks.Load(), readerHook.legacyAttempts.Load(), got.result.CommittedSeq, got.result.AcceptedCount, again.DuplicateCount)
		})
	}
}

func TestObservationReplayOrderedLocksPreserveSequence(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			missing := r.observation(1)
			missing.Observation.MonitorID = r.monitor + 1000
			invalidRevision := r.observation(3)
			invalidRevision.Observation.ConfigRevision = 0
			batch := r.batch(missing, r.observation(2), invalidRevision, r.observation(4))
			got := r.ingest(t, batch)
			if got.CommittedSeq != 4 || got.AcceptedCount != 2 || len(got.Rejected) != 2 || got.Rejected[0].Seq != 1 || got.Rejected[0].Code != "monitor_not_found" || got.Rejected[1].Seq != 3 {
				t.Fatalf("lock ordering changed source outcomes: %+v", got)
			}
			var sequences []int64
			if err := r.f.db.NewSelect().Table("probe_observations").Column("seq").Order("seq ASC").Scan(t.Context(), &sequences); err != nil {
				t.Fatal(err)
			}
			if len(sequences) != 2 || sequences[0] != 2 || sequences[1] != 4 {
				t.Fatalf("stored source sequence changed: %v", sequences)
			}
			// The overlapping prefix is checked through its durable receipts;
			// only the new observation participates in ordered authority locking.
			retry := r.batch(missing, r.observation(2), invalidRevision, r.observation(4), r.observation(5))
			again := r.ingest(t, retry)
			if again.CommittedSeq != 5 || again.AcceptedCount != 1 || again.DuplicateCount != 2 || len(again.Rejected) != 2 || replayCount(t, r.f, "probe_telemetry_receipts") != 5 || replayCount(t, r.f, "probe_observations") != 3 {
				t.Fatalf("overlapping prefix changed receipts: %+v", again)
			}
		})
	}
}
