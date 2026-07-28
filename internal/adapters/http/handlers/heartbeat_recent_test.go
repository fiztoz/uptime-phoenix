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
