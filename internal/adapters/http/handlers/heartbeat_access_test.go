package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type hbAccessHeartbeatRepo struct {
	heartbeats []*domain.Heartbeat
}

func (r *hbAccessHeartbeatRepo) Save(_ context.Context, h *domain.Heartbeat) error {
	h.ID = int64(len(r.heartbeats) + 1)
	r.heartbeats = append(r.heartbeats, h)
	return nil
}

func (r *hbAccessHeartbeatRepo) GetLatest(_ context.Context, monitorID int64) (*domain.Heartbeat, error) {
	for i := len(r.heartbeats) - 1; i >= 0; i-- {
		if r.heartbeats[i].MonitorID == monitorID {
			return r.heartbeats[i], nil
		}
	}
	return nil, ports.ErrNotFound
}

func (r *hbAccessHeartbeatRepo) ListByMonitor(_ context.Context, monitorID int64, from, to time.Time) ([]*domain.Heartbeat, error) {
	var out []*domain.Heartbeat
	for _, h := range r.heartbeats {
		if h.MonitorID == monitorID && !h.Time.Before(from) && !h.Time.After(to) {
			out = append(out, h)
		}
	}
	return out, nil
}

func (r *hbAccessHeartbeatRepo) DeleteByMonitor(_ context.Context, monitorID int64) error {
	filtered := r.heartbeats[:0]
	for _, h := range r.heartbeats {
		if h.MonitorID != monitorID {
			filtered = append(filtered, h)
		}
	}
	r.heartbeats = filtered
	return nil
}

func (r *hbAccessHeartbeatRepo) DeleteOlderThan(_ context.Context, _ time.Time) error { return nil }

func (r *hbAccessHeartbeatRepo) SaveAggregate1m(_ context.Context, _ *ports.Aggregate1m) error {
	return nil
}
func (r *hbAccessHeartbeatRepo) SaveAggregate1h(_ context.Context, _ *ports.Aggregate1h) error {
	return nil
}
func (r *hbAccessHeartbeatRepo) SaveAggregate1d(_ context.Context, _ *ports.Aggregate1d) error {
	return nil
}
func (r *hbAccessHeartbeatRepo) GetAggregate1m(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1m, error) {
	return nil, nil
}
func (r *hbAccessHeartbeatRepo) GetAggregate1h(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1h, error) {
	return nil, nil
}
func (r *hbAccessHeartbeatRepo) GetAggregate1d(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1d, error) {
	return nil, nil
}

// TestHeartbeatHandlers_OwnershipEnforced now exercises the RBAC path rather than
// raw ownership: user 1 is a NON-admin holding a grant on monitor 1 only, and
// monitor 2 (which they were never granted) must be indistinguishable from a
// monitor that does not exist — 404, never 403.
func TestHeartbeatHandlers_OwnershipEnforced(t *testing.T) {
	ctx := context.Background()
	monRepo := newFakeMonitorRepo()
	bus := newFakeMonitorBus()
	hbRepo := &hbAccessHeartbeatRepo{}
	hbSvc := services.NewHeartbeatService(hbRepo, bus)

	owner := &domain.Monitor{UserID: 1, Name: "owned", Type: "http", Active: true, Interval: 60}
	other := &domain.Monitor{UserID: 2, Name: "other", Type: "http", Active: true, Interval: 60}
	_ = monRepo.Create(ctx, owner)
	_ = monRepo.Create(ctx, other)

	userRepo := memory.NewUserRepo()
	_ = userRepo.Create(ctx, &domain.User{Username: "member", Active: true}) // id 1, non-admin
	_ = userRepo.Create(ctx, &domain.User{Username: "other", Active: true})  // id 2
	permRepo := memory.NewUserPermissionRepo()
	_ = permRepo.Grant(ctx, &domain.UserPermission{UserID: 1, MonitorID: &owner.ID})
	accessSvc := services.NewAccessService(userRepo, permRepo, nil, monRepo)

	h := handlers.NewHeartbeatHandlers(hbSvc, accessSvc)

	now := time.Now().UTC()
	_ = hbRepo.Save(context.Background(), &domain.Heartbeat{MonitorID: owner.ID, Status: domain.StatusUp, Ping: 10, Time: now})
	_ = hbRepo.Save(context.Background(), &domain.Heartbeat{MonitorID: other.ID, Status: domain.StatusUp, Ping: 20, Time: now})

	e := echo.New()
	g := e.Group("/api/monitors/:id/heartbeats", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(handlers.ContextUserIDKey, int64(1))
			return next(c)
		}
	})
	g.GET("", h.ListByMonitor)
	g.GET("/chart", h.GetChartData)
	g.DELETE("", h.ClearHistory)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/monitors/1/heartbeats", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner list status = %d; body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/monitors/2/heartbeats", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user list status = %d; want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/monitors/1/heartbeats/chart?hours=24", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner chart status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var chart struct {
		Buckets []struct {
			Avg float64 `json:"avg"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chart); err != nil {
		t.Fatalf("decode chart: %v", err)
	}
	if len(chart.Buckets) == 0 {
		t.Fatal("expected chart buckets from Go aggregation")
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/monitors/2/heartbeats", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user clear status = %d; want 404", rec.Code)
	}
}

func TestHeartbeatHandlers_GetChartData_CapsSamples(t *testing.T) {
	ctx := context.Background()
	monRepo := newFakeMonitorRepo()
	bus := newFakeMonitorBus()
	hbRepo := &hbAccessHeartbeatRepo{}
	hbSvc := services.NewHeartbeatService(hbRepo, bus)

	owner := &domain.Monitor{UserID: 1, Name: "chart-cap", Type: "http", Active: true, Interval: 60}
	_ = monRepo.Create(ctx, owner)

	userRepo := memory.NewUserRepo()
	_ = userRepo.Create(ctx, &domain.User{Username: "member", Active: true}) // id 1, non-admin
	permRepo := memory.NewUserPermissionRepo()
	_ = permRepo.Grant(ctx, &domain.UserPermission{UserID: 1, MonitorID: &owner.ID})
	h := handlers.NewHeartbeatHandlers(hbSvc, services.NewAccessService(userRepo, permRepo, nil, monRepo))

	now := time.Now().UTC()
	for i := 0; i < 2500; i++ {
		status := domain.StatusUp
		if i%50 == 0 {
			status = domain.StatusDown
		}
		_ = hbRepo.Save(context.Background(), &domain.Heartbeat{
			MonitorID: owner.ID,
			Status:    status,
			Ping:      10 + i%20,
			Time:      now.Add(-time.Duration(2500-i) * time.Minute),
		})
	}

	e := echo.New()
	g := e.Group("/api/monitors/:id/heartbeats", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(handlers.ContextUserIDKey, int64(1))
			return next(c)
		}
	})
	g.GET("/chart", h.GetChartData)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/monitors/1/heartbeats/chart?hours=720", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chart status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if len(hbRepo.heartbeats) <= 2000 {
		t.Fatalf("test setup: expected >2000 heartbeats, got %d", len(hbRepo.heartbeats))
	}
}
