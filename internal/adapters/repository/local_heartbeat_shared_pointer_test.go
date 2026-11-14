package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type sharedPointerKey struct{}
type sharedPointerHook struct {
	after     func(context.Context, *bun.QueryEvent)
	deadlocks atomic.Int64
}

func (h *sharedPointerHook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}
func (h *sharedPointerHook) AfterQuery(ctx context.Context, e *bun.QueryEvent) {
	var m *mysql.MySQLError
	if errors.As(e.Err, &m) && m.Number == 1213 {
		h.deadlocks.Add(1)
	}
	if h.after != nil {
		h.after(ctx, e)
	}
}
func sharedWait(t *testing.T, ctx context.Context, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// Exercises real ReadAppliedLocal and CommitLocalHeartbeat, not hand-written
// approximations of their locking SQL. Query barriers create the production
// active-pointer -> assignment / assignment -> active-pointer interleaving.
func TestLocalHeartbeatSharedPointerCycle(t *testing.T) {
	f := newProbeRegistryFixture(t, "mariadb")
	id := seedLocalDelivery(t, f)
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	activateLocalDelivery(t, f, f.db, meta, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	readerHasPointer := make(chan struct{})
	localHasMember := make(chan struct{})
	releaseReader := make(chan struct{})
	releaseLocal := make(chan struct{})
	var ro, lo, rr, lr sync.Once
	defer rr.Do(func() { close(releaseReader) })
	defer lr.Do(func() { close(releaseLocal) })
	h := &sharedPointerHook{}
	h.after = func(ctx context.Context, e *bun.QueryEvent) {
		if e.Err != nil {
			return
		}
		kind, _ := ctx.Value(sharedPointerKey{}).(string)
		if kind == "reader" && strings.HasPrefix(e.Query, "SELECT") && strings.Contains(e.Query, "probe_active_configs") {
			ro.Do(func() {
				close(readerHasPointer)
				select {
				case <-releaseReader:
				case <-ctx.Done():
				}
			})
		}
		if kind == "local" && strings.HasPrefix(e.Query, "SELECT") && strings.Contains(e.Query, "monitor_probe_assignments") {
			lo.Do(func() {
				close(localHasMember)
				select {
				case <-releaseLocal:
				case <-ctx.Done():
				}
			})
		}
	}
	attachInPlaceQueryHook(f.db, h)
	readerDone := make(chan error, 1)
	localDone := make(chan error, 1)
	go func() {
		_, err := activationRepo(f, f.db).(ports.LocalAppliedConfigReader).ReadAppliedLocal(context.WithValue(ctx, sharedPointerKey{}, "reader"))
		readerDone <- err
	}()
	sharedWait(t, ctx, readerHasPointer)
	go func() {
		c := localCommit(id, 0)
		c.Heartbeat.ConfigRevision = meta.Revision
		_, err := f.localHeartbeat.CommitLocalHeartbeat(context.WithValue(ctx, sharedPointerKey{}, "local"), c)
		localDone <- err
	}()
	sharedWait(t, ctx, localHasMember)
	rr.Do(func() { close(releaseReader) })
	lr.Do(func() { close(releaseLocal) })
	for _, ch := range []<-chan error{readerDone, localDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Errorf("concurrent operation: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if n := h.deadlocks.Load(); n != 0 {
		t.Fatalf("reader/local pointer cycle produced %d native deadlocks", n)
	}
	if seq := localSequence(t, f); seq != 1 {
		t.Fatalf("committed local sequence = %d", seq)
	}
	for _, table := range []string{"heartbeats", "probe_observations"} {
		var count int
		if err := f.db.NewSelect().TableExpr(table).ColumnExpr("COUNT(*)").Where("monitor_id = ?", id).Scan(ctx, &count); err != nil || count != 1 {
			t.Fatalf("persisted %s count=%d err=%v", table, count, err)
		}
	}
}

// Shared locking must still serialize pointer update and absent-pointer insert.
func TestLocalHeartbeatSharedPointerFence(t *testing.T) {
	for _, present := range []bool{true, false} {
		name := "Absent"
		if present {
			name = "Present"
		}
		t.Run(name, func(t *testing.T) {
			f := newProbeRegistryFixture(t, "mariadb")
			id := seedLocalDelivery(t, f)
			meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
			writerMeta := meta
			if present {
				activateLocalDelivery(t, f, f.db, meta, 0)
				writerMeta = prepareLocalDeliveryRevision(t, f, f.db, meta.Revision, time.Now().UTC())
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			held := make(chan struct{})
			release := make(chan struct{})
			var once, unblock sync.Once
			defer unblock.Do(func() { close(release) })
			h := &sharedPointerHook{}
			h.after = func(ctx context.Context, e *bun.QueryEvent) {
				if ctx.Value(sharedPointerKey{}) == "local" && strings.HasPrefix(e.Query, "SELECT") && strings.Contains(e.Query, "probe_active_configs") && (e.Err == nil || errors.Is(e.Err, sql.ErrNoRows)) {
					once.Do(func() {
						close(held)
						select {
						case <-release:
						case <-ctx.Done():
						}
					})
				}
			}
			attachInPlaceQueryHook(f.db, h)
			done := make(chan error, 1)
			go func() {
				c := localCommit(id, 0)
				c.Heartbeat.ConfigRevision = meta.Revision
				_, err := f.localHeartbeat.CommitLocalHeartbeat(context.WithValue(ctx, sharedPointerKey{}, "local"), c)
				done <- err
			}()
			sharedWait(t, ctx, held)
			writerConn, err := f.db.DB.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writerConn.Close() }()
			var writerID int
			if err := writerConn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&writerID); err != nil {
				t.Fatal(err)
			}
			writer := make(chan error, 1)
			go func() {
				var err error
				if present {
					_, err = writerConn.ExecContext(ctx, "UPDATE probe_active_configs SET revision = ? WHERE probe_id = ?", writerMeta.Revision, domain.LocalProbeID)
				} else {
					_, err = writerConn.ExecContext(ctx, "INSERT INTO probe_active_configs (probe_id,revision,sha256,hub_id,applied_at,assignment_count) VALUES (?,?,?,?,?,?)", domain.LocalProbeID, meta.Revision, meta.SHA256, meta.HubID, time.Now().UTC(), 1)
				}
				writer <- err
			}()
			// Confirm an actual InnoDB wait, not merely an unscheduled goroutine.
			observed := false
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				var kind, name, status string
				if err := f.db.QueryRowContext(ctx, "SHOW ENGINE INNODB STATUS").Scan(&kind, &name, &status); err != nil {
					t.Fatal(err)
				}
				for _, block := range strings.Split(status, "---TRANSACTION") {
					if strings.Contains(block, fmt.Sprintf("MariaDB thread id %d,", writerID)) && strings.Contains(block, "LOCK WAIT") && strings.Contains(block, "probe_active_configs") {
						observed = true
						break
					}
				}
				if observed {
					break
				}
				select {
				case err := <-writer:
					t.Fatalf("pointer writer escaped heartbeat lock: %v", err)
				default:
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !observed {
				t.Fatal("pointer writer did not reach a native lock wait")
			}
			unblock.Do(func() { close(release) })
			for _, ch := range []<-chan error{done, writer} {
				select {
				case err := <-ch:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if present {
				c := localCommit(id, localSequence(t, f))
				c.Heartbeat.ConfigRevision = meta.Revision
				if _, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, c); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("stale active revision accepted: %v", err)
				}
			}
		})
	}
}
