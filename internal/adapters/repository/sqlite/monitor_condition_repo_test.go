package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestMonitorConditionRepositoryPersistsLatestStateAndCursor(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()
	user := &domain.User{Username: "condition-owner", PasswordHash: "hash", Active: true}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	monitor := &domain.Monitor{
		UserID: user.ID, Name: "db", Type: "database", Active: true,
		Interval: 60, Config: map[string]any{"engine": "postgres"},
	}
	if err := repo.MonitorRepo.Create(ctx, monitor); err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	observedAt := time.Date(2031, 4, 5, 6, 7, 8, 0, time.UTC)
	used, limit, percent, threshold := 82.0, 100.0, 82.0, 80.0
	condition := &domain.MonitorCondition{
		MonitorID: monitor.ID,
		ConditionObservation: domain.ConditionObservation{
			Kind: domain.MonitorConditionSessionPool, State: domain.ConditionStateWarning,
			Used: &used, Limit: &limit, Percent: &percent, Threshold: &threshold,
			Unit: "connections", Resource: "Session pool", Scope: "cluster", Source: "pg_stat_database",
			Message: "session pool 82/100", ObservedAt: observedAt, StaleAfter: observedAt.Add(3 * time.Minute),
		},
		LastSuccessAt:    &observedAt,
		ConsecutiveState: domain.ConditionStateWarning,
		ConsecutiveCount: 2,
	}
	if err := repo.MonitorConditionRepo.Upsert(ctx, condition); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := repo.MonitorConditionRepo.Get(ctx, monitor.ID, domain.MonitorConditionSessionPool)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != domain.ConditionStateWarning || got.Percent == nil || *got.Percent != 82 || got.ConsecutiveCount != 2 {
		t.Fatalf("persisted condition=%+v", got)
	}
	if got.ObservedAt.Location() != time.UTC || got.StaleAfter.Location() != time.UTC {
		t.Fatalf("condition times are not UTC: observed=%v stale=%v", got.ObservedAt.Location(), got.StaleAfter.Location())
	}

	notifiedAt := observedAt.Add(time.Second)
	condition.LastNotifiedState = domain.ConditionStateWarning
	condition.LastNotifiedAt = &notifiedAt
	condition.ConsecutiveCount = 3
	if err := repo.MonitorConditionRepo.Upsert(ctx, condition); err != nil {
		t.Fatalf("upsert cursor: %v", err)
	}
	got, err = repo.MonitorConditionRepo.Get(ctx, monitor.ID, domain.MonitorConditionSessionPool)
	if err != nil {
		t.Fatalf("get cursor: %v", err)
	}
	if got.LastNotifiedState != domain.ConditionStateWarning || got.LastNotifiedAt == nil || got.ConsecutiveCount != 3 {
		t.Fatalf("persisted cursor=%+v", got)
	}

	rows, err := repo.MonitorConditionRepo.ListByMonitorIDs(ctx, []int64{monitor.ID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("list scoped rows=%d err=%v", len(rows), err)
	}
	empty, err := repo.MonitorConditionRepo.ListByMonitorIDs(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty scope rows=%d err=%v", len(empty), err)
	}

	if err := repo.MonitorRepo.Delete(ctx, monitor.ID); err != nil {
		t.Fatalf("delete monitor: %v", err)
	}
	_, err = repo.MonitorConditionRepo.Get(ctx, monitor.ID, domain.MonitorConditionSessionPool)
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("condition survived monitor cascade: %v", err)
	}
}
