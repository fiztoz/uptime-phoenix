package repository_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"
)

// runtimeInsertBarrier holds actual repository transactions after their missing-row
// reads. It does not synthesize an error or change any SQL/production behavior.
type runtimeInsertBarrier struct {
	deadlocks atomic.Int64
	once      sync.Once
	arrived   chan<- struct{}
	release   <-chan struct{}
}

func (h *runtimeInsertBarrier) BeforeQuery(ctx context.Context, e *bun.QueryEvent) context.Context {
	if strings.HasPrefix(e.Query, "INSERT INTO `probe_runtime_owners`") {
		h.once.Do(func() {
			select {
			case h.arrived <- struct{}{}:
			case <-ctx.Done():
				return
			}
			select {
			case <-h.release:
			case <-ctx.Done():
			}
		})
	}
	return ctx
}
func (h *runtimeInsertBarrier) AfterQuery(_ context.Context, e *bun.QueryEvent) {
	var native *mysql.MySQLError
	if errors.As(e.Err, &native) && native.Number == 1213 {
		h.deadlocks.Add(1)
	}
}

// TestProbeRuntimeNativeCrossProbeGapDeadlock requires a real InnoDB1213 victim
// from two distinct probe cold starts, then verifies automatic whole-transaction retry.
// Run only against a disposable TEST_MARIADB_DSN: the standard fixture resets it.
func TestProbeRuntimeNativeCrossProbeGapDeadlock(t *testing.T) {
	f := newProbeRegistryFixture(t, "mariadb")
	f.remote(t, probeRegistryID1, "gap-a")
	f.remote(t, probeRegistryID2, "gap-b")
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	var version, isolation string
	if err := f.db.NewRaw("SELECT VERSION(),@@tx_isolation").Scan(ctx, &version, &isolation); err != nil {
		t.Fatal(err)
	}
	t.Logf("native engine=%s isolation=%s", version, isolation)
	if isolation != "REPEATABLE-READ" {
		t.Fatal("native missing-row gap premise requires REPEATABLE-READ")
	}
	ids := []string{probeRegistryID1, probeRegistryID2}
	owners := []string{probeRegistryID2, probeRegistryID3}
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var workers sync.WaitGroup
	defer func() { cancel(); unblock(); workers.Wait() }()
	stores := make([]*repository.ProbeConnectorStore, 2)
	hooks := make([]*runtimeInsertBarrier, 2)
	type outcome struct {
		index int
		lease domain.ProbeRuntimeLease
		err   error
	}
	done := make(chan outcome, 2)
	for i := range stores {
		db := reopenConfigDB(t, f)
		db.SetMaxOpenConns(1)
		hooks[i] = &runtimeInsertBarrier{arrived: arrived, release: release}
		attachInPlaceQueryHook(db, hooks[i])
		stores[i] = repository.NewProbeConnectorStore(db)
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			lease, err := stores[i].AcquireRuntime(ctx, ids[i], owners[i])
			done <- outcome{i, lease, err}
		}(i)
	}
	for n := 0; n < 2; n++ {
		select {
		case <-arrived:
		case early := <-done:
			cancel()
			unblock()
			t.Fatalf("inconclusive: call%d completed before both INSERT barriers: %v", early.index, early.err)
		case <-ctx.Done():
			unblock()
			t.Fatal("inconclusive: both native transactions did not reach INSERT barrier")
		}
	}
	unblock()
	results := make([]outcome, 2)
	for n := 0; n < 2; n++ {
		select {
		case r := <-done:
			results[r.index] = r
		case <-ctx.Done():
			t.Fatal("native calls exceeded bounded context")
		}
	}
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("call %d did not recover internally: %v", i, r.err)
		}
	}
	deadlocks := hooks[0].deadlocks.Load() + hooks[1].deadlocks.Load()
	if deadlocks == 0 {
		t.Fatal("inconclusive: no actual native1213 observed")
	}
	t.Logf("actual native1213 events=%d; both calls recovered without caller retry", deadlocks)
	for i, r := range results {
		if r.lease.Epoch != 1 || r.lease.ProbeID != ids[i] || r.lease.OwnerID != owners[i] {
			t.Fatalf("wrong authority after recovery: %+v", r.lease)
		}
		if _, err := stores[i].AcquireRuntime(ctx, ids[i], owners[i]); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("duplicate live acquisition bypassed fence: %v", err)
		}
		stale := r.lease
		stale.Epoch++
		if _, err := stores[i].RenewRuntime(ctx, stale); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("stale renewal bypassed fence: %v", err)
		}
		if err := stores[i].ReleaseRuntime(ctx, stale); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("stale release bypassed fence: %v", err)
		}
		renewed, err := stores[i].RenewRuntime(ctx, r.lease)
		if err != nil || renewed.Epoch != 1 {
			t.Fatalf("valid authority damaged: %+v %v", renewed, err)
		}
		if err := stores[i].ReleaseRuntime(ctx, renewed); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("native1213 internal whole-transaction retry, epoch1 rollback, duplicate ownership and stale fencing verified")
}
