package repository_test

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

type probeLockAttemptHook struct {
	once      sync.Once
	attempted chan struct{}
}

func (h *probeLockAttemptHook) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	if strings.HasPrefix(event.Query, "UPDATE `probes`") {
		h.once.Do(func() { close(h.attempted) })
	}
	return ctx
}
func (*probeLockAttemptHook) AfterQuery(context.Context, *bun.QueryEvent) {}

func TestMonitorCreateUsesConfigurationLockOrder(t *testing.T) {
	f := newProbeRegistryFixture(t, "mariadb")
	userID := f.user(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := f.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	if err := tx.NewSelect().Table("probes").Column("id").Where("id = ?", domain.LocalProbeID).Scan(ctx, &id); err != nil {
		t.Fatal(err)
	}
	peer := reopenConfigDB(t, f)
	hook := &probeLockAttemptHook{attempted: make(chan struct{})}
	peer.AddQueryHook(hook)
	result := make(chan error, 1)
	monitor := &domain.Monitor{UserID: userID, Name: "create while source is read", Type: "http", Active: true, Interval: 60, Timeout: 10, Config: map[string]any{"url": "https://example.test"}}
	go func() { result <- mariadb.NewMonitorRepo(peer).Create(ctx, monitor) }()
	select {
	case <-hook.attempted:
	case <-ctx.Done():
		t.Fatal("writer did not reach probe lock")
	}
	// With the old order the writer already held an inserted monitor row and
	// waited for our probe lock. This read completes the deadlock deterministically.
	if _, err := tx.NewSelect().Model((*repository.MonitorModel)(nil)).Count(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatalf("configuration read broke monitor creation: %v", err)
	}
	if monitor.ID == 0 {
		t.Fatal("monitor creation did not commit")
	}
	if _, err := f.assignments.GetByMonitorID(ctx, monitor.ID); err != nil {
		t.Fatal(err)
	}
}
