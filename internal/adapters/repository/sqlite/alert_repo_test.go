package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestAlertRepo_OpenAckResolveLifecycle(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "alerts.db") + "?cache=shared"
	db, err := NewDB(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := repository.RunMigrations(db.DB, "sqlite"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	users := NewUserRepo(db)
	monitors := NewMonitorRepo(db)
	alerts := NewAlertRepo(db)
	ctx := context.Background()

	u := &domain.User{Username: "ops", PasswordHash: "x", IsAdmin: true}
	if err := users.Create(ctx, u); err != nil {
		t.Fatalf("user: %v", err)
	}
	m := &domain.Monitor{
		UserID: u.ID, Name: "api", Type: "http", Active: true,
		Interval: 60, MaxRetries: 0, RetryInterval: 0,
	}
	if err := monitors.Create(ctx, m); err != nil {
		t.Fatalf("monitor: %v", err)
	}

	mid := m.ID
	now := time.Now().UTC().Truncate(time.Second)
	a := &domain.Alert{
		MonitorID:     m.ID,
		Status:        domain.AlertStatusFiring,
		Message:       "api is DOWN",
		FiredAt:       now,
		AckToken:      "token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		OpenMonitorID: &mid,
	}
	if err := alerts.Create(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.ID == 0 {
		t.Fatal("expected id assigned")
	}

	// Second open for same monitor must conflict.
	dup := &domain.Alert{
		MonitorID: m.ID, Status: domain.AlertStatusFiring, Message: "x",
		FiredAt: now, AckToken: "token-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		OpenMonitorID: &mid,
	}
	if err := alerts.Create(ctx, dup); err != ports.ErrConflict {
		t.Fatalf("second open err = %v; want ErrConflict", err)
	}

	open, err := alerts.GetOpenByMonitorID(ctx, m.ID)
	if err != nil {
		t.Fatalf("GetOpenByMonitorID: %v", err)
	}
	if open.ID != a.ID {
		t.Fatalf("open id = %d; want %d", open.ID, a.ID)
	}

	ackedAt := now.Add(time.Minute)
	open.Status = domain.AlertStatusAcked
	open.AckedAt = &ackedAt
	if err := alerts.Update(ctx, open); err != nil {
		t.Fatalf("ack update: %v", err)
	}

	byTok, err := alerts.GetByAckToken(ctx, a.AckToken)
	if err != nil {
		t.Fatalf("GetByAckToken: %v", err)
	}
	if byTok.Status != domain.AlertStatusAcked {
		t.Fatalf("status = %s; want acked", byTok.Status)
	}

	resolvedAt := now.Add(2 * time.Hour)
	open.Status = domain.AlertStatusResolved
	open.ResolvedAt = &resolvedAt
	open.OpenMonitorID = nil
	if err := alerts.Update(ctx, open); err != nil {
		t.Fatalf("resolve update: %v", err)
	}
	if _, err := alerts.GetOpenByMonitorID(ctx, m.ID); err != ports.ErrNotFound {
		t.Fatalf("after resolve GetOpen = %v; want ErrNotFound", err)
	}

	// A new open is allowed after resolve.
	mid2 := m.ID
	next := &domain.Alert{
		MonitorID: m.ID, Status: domain.AlertStatusFiring, Message: "again",
		FiredAt: now.Add(3 * time.Hour), AckToken: "token-cccccccccccccccccccccccccccccc",
		OpenMonitorID: &mid2,
	}
	if err := alerts.Create(ctx, next); err != nil {
		t.Fatalf("create after resolve: %v", err)
	}

	list, err := alerts.List(ctx, ports.AlertFilter{OpenOnly: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != next.ID {
		t.Fatalf("open list = %+v; want one row id=%d", list, next.ID)
	}
}
