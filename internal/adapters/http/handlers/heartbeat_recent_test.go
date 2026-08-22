package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// The heartbeat list must return the LATEST `limit` beats in the window, whatever
// order it is asked to present them in.
//
// Regression guard: the handler sorted first and truncated second, so an ascending
// request served the OLDEST rows. The monitor detail page fetches its "Recent
// Checks" bar with hours=24&limit=60&order=asc — on a monitor checked every 10s
// that is the first 10 minutes of the day, hours stale. A monitor that had gone
// down still showed 60 green bars after every page load or edit-triggered reload;
// only the live WebSocket beats appended to the tail were red.
func TestHeartbeatHandlers_ListReturnsMostRecentBeats_WhenOrderedAscending(t *testing.T) {
	monRepo := newFakeMonitorRepo()
	bus := newFakeMonitorBus()

	mon := &domain.Monitor{UserID: 1, Name: "m", Type: "http", Active: true, Interval: 10}
	if err := monRepo.Create(context.Background(), mon); err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	hbRepo := &hbWindowRepo{}
	hbSvc := services.NewHeartbeatService(hbRepo, bus)

	// 90 UP beats earlier today, then 20 DOWN beats — the monitor is down NOW.
	now := time.Now().UTC()
	for i := range 90 {
		_ = hbRepo.Save(context.Background(), &domain.Heartbeat{
			MonitorID: mon.ID, Status: domain.StatusUp, Ping: 120,
			Time: now.Add(-6*time.Hour + time.Duration(i)*10*time.Second),
		})
	}
	for i := range 20 {
		_ = hbRepo.Save(context.Background(), &domain.Heartbeat{
			MonitorID: mon.ID, Status: domain.StatusDown,
			Time: now.Add(-200*time.Second + time.Duration(i)*10*time.Second),
		})
	}

	userRepo := memory.NewUserRepo()
	_ = userRepo.Create(context.Background(), &domain.User{Username: "admin", Active: true, IsAdmin: true})
	accessSvc := services.NewAccessService(userRepo, memory.NewUserPermissionRepo(), nil, monRepo)

	h := handlers.NewHeartbeatHandlers(hbSvc, accessSvc)
	e := echo.New()
	e.HideBanner, e.HidePort = true, true
	g := e.Group("/api/monitors/:id/heartbeats", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(handlers.ContextUserIDKey, int64(1))
			return next(c)
		}
	})
	g.GET("", h.ListByMonitor)

	req := httptest.NewRequest(http.MethodGet, "/api/monitors/1/heartbeats?hours=24&limit=60&order=asc", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET heartbeats returned %d (body: %s)", rec.Code, rec.Body.String())
	}
	var got []struct {
		Status string    `json:"status"`
		Time   time.Time `json:"time"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 60 {
		t.Fatalf("returned %d heartbeats; want 60", len(got))
	}

	// The 20 most recent beats are DOWN, so the tail of an ascending page must be down.
	down := 0
	for _, hb := range got {
		if hb.Status == "down" {
			down++
		}
	}
	if down != 20 {
		t.Errorf("page contains %d down beats; want 20 — the newest 60 beats end in the "+
			"20 down ones, so the bar must show red, not a full row of green", down)
	}
	if last := got[len(got)-1]; last.Status != "down" {
		t.Errorf("last beat of the ascending page is %q at %s; want the newest beat (down)",
			last.Status, last.Time.Format(time.RFC3339))
	}
	// Ascending order is still honored within the page.
	for i := 1; i < len(got); i++ {
		if got[i].Time.Before(got[i-1].Time) {
			t.Fatalf("page is not in ascending time order at index %d", i)
		}
	}
}

// countingRecentRepo records whether the list handler used the SQL-limited
// path or fell back to loading the whole window.
type countingRecentRepo struct {
	mu      sync.Mutex
	rows    []*domain.Heartbeat
	list    int
	recent  int
	lastLim int
}

func (r *countingRecentRepo) Save(_ context.Context, h *domain.Heartbeat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, h)
	return nil
}
func (r *countingRecentRepo) GetLatest(context.Context, int64) (*domain.Heartbeat, error) {
	return nil, ports.ErrNotFound
}
func (r *countingRecentRepo) ListByMonitor(_ context.Context, monitorID int64, from, to time.Time) ([]*domain.Heartbeat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list++
	return r.window(monitorID, from, to), nil
}
func (r *countingRecentRepo) ListRecentByMonitor(_ context.Context, monitorID int64, from, to time.Time, limit int) ([]*domain.Heartbeat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recent++
	r.lastLim = limit
	out := r.window(monitorID, from, to)
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	// Newest first, matching the MariaDB/SQLite adapters.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
func (r *countingRecentRepo) window(monitorID int64, from, to time.Time) []*domain.Heartbeat {
	var out []*domain.Heartbeat
	for _, h := range r.rows {
		if h.MonitorID == monitorID && !h.Time.Before(from) && !h.Time.After(to) {
			cp := *h
			out = append(out, &cp)
		}
	}
	return out
}
func (r *countingRecentRepo) DeleteByMonitor(context.Context, int64) error { return nil }
func (r *countingRecentRepo) DeleteOlderThan(context.Context, time.Time) error {
	return nil
}
func (r *countingRecentRepo) SaveAggregate1m(context.Context, *ports.Aggregate1m) error {
	return nil
}
func (r *countingRecentRepo) SaveAggregate1h(context.Context, *ports.Aggregate1h) error {
	return nil
}
func (r *countingRecentRepo) SaveAggregate1d(context.Context, *ports.Aggregate1d) error {
	return nil
}
func (r *countingRecentRepo) GetAggregate1m(context.Context, int64, time.Time) ([]*ports.Aggregate1m, error) {
	return nil, nil
}
func (r *countingRecentRepo) GetAggregate1h(context.Context, int64, time.Time) ([]*ports.Aggregate1h, error) {
	return nil, nil
}
func (r *countingRecentRepo) GetAggregate1d(context.Context, int64, time.Time) ([]*ports.Aggregate1d, error) {
	return nil, nil
}

func TestHeartbeatHandlers_ListUsesRecentReaderWhenLimitSet(t *testing.T) {
	monRepo := newFakeMonitorRepo()
	bus := newFakeMonitorBus()
	mon := &domain.Monitor{UserID: 1, Name: "m", Type: "http", Active: true, Interval: 60}
	if err := monRepo.Create(context.Background(), mon); err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	hbRepo := &countingRecentRepo{}
	now := time.Now().UTC()
	for i := range 20 {
		_ = hbRepo.Save(context.Background(), &domain.Heartbeat{
			ID: int64(i + 1), MonitorID: mon.ID, Status: domain.StatusUp, Ping: 10,
			Time: now.Add(-time.Duration(19-i) * time.Minute),
		})
	}
	hbSvc := services.NewHeartbeatService(hbRepo, bus)
	userRepo := memory.NewUserRepo()
	_ = userRepo.Create(context.Background(), &domain.User{Username: "admin", Active: true, IsAdmin: true})
	accessSvc := services.NewAccessService(userRepo, memory.NewUserPermissionRepo(), nil, monRepo)

	h := handlers.NewHeartbeatHandlers(hbSvc, accessSvc)
	e := echo.New()
	e.HideBanner, e.HidePort = true, true
	g := e.Group("/api/monitors/:id/heartbeats", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(handlers.ContextUserIDKey, int64(1))
			return next(c)
		}
	})
	g.GET("", h.ListByMonitor)

	req := httptest.NewRequest(http.MethodGet, "/api/monitors/1/heartbeats?hours=6&limit=60&order=asc", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET heartbeats returned %d (body: %s)", rec.Code, rec.Body.String())
	}
	if hbRepo.recent != 1 {
		t.Errorf("ListRecentByMonitor calls = %d; want 1 (SQL-limited path)", hbRepo.recent)
	}
	if hbRepo.list != 0 {
		t.Errorf("ListByMonitor calls = %d; want 0 — the window must not be fully loaded", hbRepo.list)
	}
	if hbRepo.lastLim != 60 {
		t.Errorf("limit passed to repo = %d; want 60", hbRepo.lastLim)
	}
}
