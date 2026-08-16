package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type conditionAccessRepo struct {
	rows map[string]*domain.MonitorCondition
}

func newConditionAccessRepo(rows ...*domain.MonitorCondition) *conditionAccessRepo {
	repo := &conditionAccessRepo{rows: make(map[string]*domain.MonitorCondition)}
	for _, row := range rows {
		repo.rows[conditionAccessKey(row.MonitorID, row.Kind)] = row
	}
	return repo
}

func conditionAccessKey(monitorID int64, kind string) string {
	return strconv.FormatInt(monitorID, 10) + ":" + kind
}

func (r *conditionAccessRepo) Upsert(_ context.Context, condition *domain.MonitorCondition) error {
	r.rows[conditionAccessKey(condition.MonitorID, condition.Kind)] = condition
	return nil
}

func (r *conditionAccessRepo) Get(_ context.Context, monitorID int64, kind string) (*domain.MonitorCondition, error) {
	condition, ok := r.rows[conditionAccessKey(monitorID, kind)]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return condition, nil
}

func (r *conditionAccessRepo) ListAll(_ context.Context) ([]*domain.MonitorCondition, error) {
	rows := make([]*domain.MonitorCondition, 0, len(r.rows))
	for _, row := range r.rows {
		rows = append(rows, row)
	}
	return rows, nil
}

func (r *conditionAccessRepo) ListByMonitorIDs(_ context.Context, monitorIDs []int64) ([]*domain.MonitorCondition, error) {
	allowed := make(map[int64]bool, len(monitorIDs))
	for _, id := range monitorIDs {
		allowed[id] = true
	}
	rows := make([]*domain.MonitorCondition, 0)
	for _, row := range r.rows {
		if allowed[row.MonitorID] {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (r *conditionAccessRepo) DeleteKind(_ context.Context, monitorID int64, kind string) error {
	delete(r.rows, conditionAccessKey(monitorID, kind))
	return nil
}

func (r *conditionAccessRepo) DeleteByMonitor(_ context.Context, monitorID int64) error {
	for key, row := range r.rows {
		if row.MonitorID == monitorID {
			delete(r.rows, key)
		}
	}
	return nil
}

func TestMonitorConditionHandlersScopeRowsAndUseExplicitWireShape(t *testing.T) {
	ctx := context.Background()
	monitorRepo := newFakeMonitorRepo()
	visible := &domain.Monitor{UserID: 1, Name: "visible", Type: "database", Active: true, Interval: 60}
	hidden := &domain.Monitor{UserID: 2, Name: "hidden", Type: "database", Active: true, Interval: 60}
	if err := monitorRepo.Create(ctx, visible); err != nil {
		t.Fatal(err)
	}
	if err := monitorRepo.Create(ctx, hidden); err != nil {
		t.Fatal(err)
	}

	userRepo := memory.NewUserRepo()
	if err := userRepo.Create(ctx, &domain.User{Username: "member", Active: true}); err != nil {
		t.Fatal(err)
	}
	permissionRepo := memory.NewUserPermissionRepo()
	if err := permissionRepo.Grant(ctx, &domain.UserPermission{UserID: 1, MonitorID: &visible.ID}); err != nil {
		t.Fatal(err)
	}
	access := services.NewAccessService(userRepo, permissionRepo, nil, monitorRepo)

	now := time.Now().UTC()
	repo := newConditionAccessRepo(
		conditionAccessRow(visible.ID, now),
		conditionAccessRow(hidden.ID, now),
	)
	service := services.NewMonitorConditionService(repo, nil, nil, nil)
	handler := handlers.NewMonitorConditionHandlers(service, access)

	e := echo.New()
	group := e.Group("/api/monitor-conditions", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(handlers.ContextUserIDKey, int64(1))
			return next(c)
		}
	})
	group.GET("", handler.List)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/monitor-conditions", nil)
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(rows) != 1 || int64(rows[0]["monitor_id"].(float64)) != visible.ID {
		t.Fatalf("scoped rows=%v", rows)
	}
	if _, leaked := rows[0]["LastNotifiedState"]; leaked {
		t.Fatalf("internal notification cursor leaked: %v", rows[0])
	}
	if _, ok := rows[0]["stale_after"]; !ok {
		t.Fatalf("explicit snake-case view missing stale_after: %v", rows[0])
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/monitor-conditions?monitor_id="+strconv.FormatInt(hidden.ID, 10), nil)
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("hidden monitor query status=%d, want 404", recorder.Code)
	}
}

func TestMonitorConditionHandlersOmitUnconfirmedState(t *testing.T) {
	ctx := context.Background()
	monitorRepo := newFakeMonitorRepo()
	monitor := &domain.Monitor{UserID: 1, Name: "db", Type: "database", Active: true, Interval: 60}
	if err := monitorRepo.Create(ctx, monitor); err != nil {
		t.Fatal(err)
	}
	userRepo := memory.NewUserRepo()
	if err := userRepo.Create(ctx, &domain.User{Username: "admin", IsAdmin: true, Active: true}); err != nil {
		t.Fatal(err)
	}
	access := services.NewAccessService(userRepo, memory.NewUserPermissionRepo(), nil, monitorRepo)

	now := time.Now().UTC()
	unconfirmed := conditionAccessRow(monitor.ID, now)
	unconfirmed.State = ""
	unconfirmed.ConsecutiveCount = 1
	unconfirmed.LastNotifiedState = ""
	repo := newConditionAccessRepo(unconfirmed)
	handler := handlers.NewMonitorConditionHandlers(services.NewMonitorConditionService(repo, nil, nil, nil), access)

	e := echo.New()
	group := e.Group("/api/monitor-conditions", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(handlers.ContextUserIDKey, int64(1))
			return next(c)
		}
	})
	group.GET("", handler.List)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/monitor-conditions", nil)
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("unconfirmed rows leaked to REST: %v", rows)
	}
}

func conditionAccessRow(monitorID int64, now time.Time) *domain.MonitorCondition {
	percent := 88.0
	threshold := 80.0
	return &domain.MonitorCondition{
		MonitorID: monitorID,
		ConditionObservation: domain.ConditionObservation{
			Kind: domain.MonitorConditionStorage, State: domain.ConditionStateWarning,
			Percent: &percent, Threshold: &threshold, Unit: "bytes", Resource: "Database size",
			Scope: "database", Source: "fixed query", Message: "capacity warning",
			ObservedAt: now, StaleAfter: now.Add(3 * time.Minute),
		},
		ConsecutiveState:  domain.ConditionStateWarning,
		ConsecutiveCount:  2,
		LastNotifiedState: domain.ConditionStateWarning,
	}
}
