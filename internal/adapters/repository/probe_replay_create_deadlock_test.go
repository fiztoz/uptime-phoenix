package repository_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// A 1000-monitor load run returned HTTP 500 on monitor create with MariaDB
// 1213. Remote replay range-locks assignment history and only later asks for
// the local registration, through the FK check of the overall dirty bucket.
// Create holds that registration exclusively and then inserts a new monitor's
// history row into the gap replay has locked. Create has no retry, so it was
// the deadlock victim. Local configuration activation (the probe
// configuration refresh) holds the registration and then wants the
// installation row replay has read. This pins both interleavings and
// requires no 1213 from either side. Snapshot save and assignment replacement
// did not collide here on the old order; they check the new order does not
// add a cycle with them.
func TestObservationReplayDoesNotDeadlockLocalRegistrationWriters(t *testing.T) {
	t.Run("monitor_create", func(t *testing.T) {
		testReplayVersusLocalWriter(t, true, func(ctx context.Context, t *testing.T, r replayFixture, peer *bun.DB) (func(context.Context) error, func(context.Context, *testing.T)) {
			created := &domain.Monitor{UserID: r.f.user(t), Name: "created during replay", Type: "http", Active: true, Interval: 60, Timeout: 5, Config: map[string]any{}}
			return func(ctx context.Context) error { return mariadb.NewMonitorRepo(peer).Create(ctx, created) },
				func(ctx context.Context, t *testing.T) {
					var assigned int
					if err := r.f.db.NewSelect().Table("monitor_probe_assignments").ColumnExpr("COUNT(*)").Where("monitor_id = ? AND probe_id = ? AND active = ?", created.ID, domain.LocalProbeID, true).Scan(ctx, &assigned); err != nil || assigned != 1 {
						t.Fatalf("created monitor %d local assignments=%d err=%v", created.ID, assigned, err)
					}
				}
		})
	})
	t.Run("local_config_activation", func(t *testing.T) {
		testReplayVersusLocalWriter(t, true, func(ctx context.Context, t *testing.T, r replayFixture, peer *bun.DB) (func(context.Context) error, func(context.Context, *testing.T)) {
			builder, _ := sourceConfigBuilder(t, r.f, r.f.db)
			local, err := builder.Prepare(ctx, r.session.HubID, 0, r.at, r.at)
			if err != nil {
				t.Fatal(err)
			}
			params := ports.LocalActivationParams{Target: local.ProbeConfigTarget, Revision: local.Revision, SHA256: local.SHA256, AppliedAt: r.at}
			activation := repository.NewProbeActivationStore(peer, probe.LocalConfigEncoder{})
			return func(ctx context.Context) error { _, err := activation.ActivateLocal(ctx, params); return err },
				func(ctx context.Context, t *testing.T) {
					var revision int64
					if err := r.f.db.NewSelect().Table("probe_active_configs").Column("revision").Where("probe_id = ?", domain.LocalProbeID).Scan(ctx, &revision); err != nil || revision != local.Revision {
						t.Fatalf("active local revision=%d want %d err=%v", revision, local.Revision, err)
					}
				}
		})
	})
	t.Run("local_config_save", func(t *testing.T) {
		testReplayVersusLocalWriter(t, false, func(ctx context.Context, t *testing.T, r replayFixture, peer *bun.DB) (func(context.Context) error, func(context.Context, *testing.T)) {
			builder, _ := sourceConfigBuilder(t, r.f, peer)
			var saved *domain.ProbeConfigMetadata
			return func(ctx context.Context) error {
					local, err := builder.Prepare(ctx, r.session.HubID, 0, r.at, r.at)
					if err == nil {
						saved = &local
					}
					return err
				},
				func(ctx context.Context, t *testing.T) {
					if saved == nil || replayCount(t, r.f, "probe_config_snapshots") < 2 {
						t.Fatalf("local snapshot not saved: %+v", saved)
					}
				}
		})
	})
	t.Run("assignment_replace", func(t *testing.T) {
		testReplayVersusLocalWriter(t, false, func(ctx context.Context, t *testing.T, r replayFixture, peer *bun.DB) (func(context.Context) error, func(context.Context, *testing.T)) {
			monitor := localMonitor(t, r.f)
			store := repository.NewProbeAssignmentStore(peer)
			return func(ctx context.Context) error {
					_, err := store.Replace(ctx, monitor, 1, []string{r.session.ProbeID, domain.LocalProbeID}, domain.HealthPolicyAnyDown)
					return err
				},
				func(ctx context.Context, t *testing.T) {
					got, err := r.f.assignments.GetByMonitorID(ctx, monitor)
					if err != nil || got.Revision != 2 || len(got.Assignments) != 2 {
						t.Fatalf("replaced assignments=%+v err=%v", got, err)
					}
				}
		})
	})
}

type localRegistrationWriter func(context.Context, *testing.T, replayFixture, *bun.DB) (run func(context.Context) error, verify func(context.Context, *testing.T))

// mustWait marks writers that collided with replay on the old lock order; the
// others are ordering checks that may also finish before replay resumes.
func testReplayVersusLocalWriter(t *testing.T, mustWait bool, setup localRegistrationWriter) {
	r := newReplayFixture(t, "mariadb")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// Fixture writes may already own this minute's overall bucket; a duplicate
	// key then skips the FK check that requests the registration lock.
	if _, err := r.f.db.ExecContext(ctx, "DELETE FROM probe_dirty_buckets"); err != nil {
		t.Fatal(err)
	}
	peerDB := reopenConfigDB(t, r.f)
	peerDB.SetMaxOpenConns(1)
	observer := reopenConfigDB(t, r.f)
	peerConn := connectionID(ctx, t, peerDB)
	runPeer, verifyPeer := setup(ctx, t, r, peerDB)

	reached, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	// Pause replay after its evidence insert: authority and history reads are
	// locked, and the dirty-bucket marks are still to come.
	replayHook := &replayAssignmentLockHook{after: func(ctx context.Context, e *bun.QueryEvent) {
		if e.Err == nil && strings.HasPrefix(e.Query, "INSERT INTO `probe_observations`") {
			once.Do(func() {
				close(reached)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}
	}}
	peerHook := &replayAssignmentLockHook{}
	attachInPlaceQueryHook(r.f.db, replayHook)
	attachInPlaceQueryHook(peerDB, peerHook)

	replayDone, peerDone := make(chan error, 1), make(chan error, 1)
	go func() {
		got, err := r.store.IngestReplayBatch(ctx, r.session, r.batch(r.observation(1)), &services.AccessService{})
		if err == nil && (got.CommittedSeq != 1 || got.AcceptedCount != 1 || len(got.Rejected) != 0) {
			err = fmt.Errorf("bad receipt: %+v", got)
		}
		replayDone <- err
	}()
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatal("replay did not reach its evidence insert", ctx.Err())
	}
	go func() { peerDone <- runPeer(ctx) }()
	finished, peerErr := waitForInnoDBLockWait(ctx, t, observer, peerConn, peerDone)
	if finished && mustWait {
		t.Fatalf("connection %d finished without waiting for replay: %v", peerConn, peerErr)
	}
	unblock()
	replayErr := <-replayDone
	if !finished {
		peerErr = <-peerDone
	}
	t.Logf("replay_1213=%d peer_1213=%d replay_err=%v peer_err=%v", replayHook.deadlocks.Load(), peerHook.deadlocks.Load(), replayErr, peerErr)
	if replayErr != nil || peerErr != nil {
		t.Fatalf("operations failed: replay=%v peer=%v", replayErr, peerErr)
	}
	if n := replayHook.deadlocks.Load() + peerHook.deadlocks.Load(); n != 0 {
		t.Fatalf("observation replay and the local registration writer deadlocked: 1213 x%d", n)
	}
	if n := replayCount(t, r.f, "probe_observations"); n != 1 {
		t.Fatalf("observations=%d", n)
	}
	var overall int
	if err := r.f.db.NewSelect().Table("probe_dirty_buckets").ColumnExpr("COUNT(*)").Where("monitor_id = ? AND probe_id = ? AND resolution = ?", r.monitor, domain.LocalProbeID, domain.DirtyResolutionOverall).Scan(ctx, &overall); err != nil || overall != 1 {
		t.Fatalf("replayed monitor overall dirty buckets=%d err=%v", overall, err)
	}
	verifyPeer(ctx, t)
}

func connectionID(ctx context.Context, t *testing.T, db *bun.DB) int64 {
	t.Helper()
	var id int64
	if err := db.NewRaw("SELECT CONNECTION_ID()").Scan(ctx, &id); err != nil {
		t.Fatal(err)
	}
	return id
}

// waitForInnoDBLockWait observes a real lock wait in InnoDB status instead of
// sleeping, so the paused transaction is released only once the peer queues.
// It reports finished with the peer's result if the peer never had to wait.
func waitForInnoDBLockWait(ctx context.Context, t *testing.T, observer *bun.DB, connection int64, done <-chan error) (bool, error) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var kind, name, status sql.NullString
		if err := observer.QueryRowContext(ctx, "SHOW ENGINE INNODB STATUS").Scan(&kind, &name, &status); err != nil {
			t.Fatal(err)
		}
		for _, block := range strings.Split(status.String, "---TRANSACTION") {
			if strings.Contains(block, fmt.Sprintf("MariaDB thread id %d,", connection)) && strings.Contains(block, "LOCK WAIT") {
				return false, nil
			}
		}
		select {
		case err := <-done:
			return true, err
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("connection %d did not wait for replay: %v", connection, ctx.Err())
		}
	}
}
