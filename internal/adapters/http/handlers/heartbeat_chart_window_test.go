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

// TestHeartbeatHandlers_GetChartData_Covers24hAt30s pins the authenticated half
// of issue #195: a 30 s monitor writes 2880 beats in 24 h, so the old
// 2000-sample cap charted only the newest ~16.7 h of the "24 h" range.
func TestHeartbeatHandlers_GetChartData_Covers24hAt30s(t *testing.T) {
	ctx := context.Background()
	monRepo := newFakeMonitorRepo()
	hbRepo := &hbAccessHeartbeatRepo{}
	hbSvc := services.NewHeartbeatService(hbRepo, newFakeMonitorBus())

	owner := &domain.Monitor{UserID: 1, Name: "chart-window", Type: "http", Active: true, Interval: 30}
	_ = monRepo.Create(ctx, owner)
	userRepo := memory.NewUserRepo()
	_ = userRepo.Create(ctx, &domain.User{Username: "member", Active: true})
	permRepo := memory.NewUserPermissionRepo()
	_ = permRepo.Grant(ctx, &domain.UserPermission{UserID: 1, MonitorID: &owner.ID})
	h := handlers.NewHeartbeatHandlers(hbSvc, services.NewAccessService(userRepo, permRepo, nil, monRepo))

	now := time.Now().UTC().Truncate(time.Second)
	for k := 25 * 120; k >= 0; k-- {
		_ = hbRepo.Save(ctx, &domain.Heartbeat{
			MonitorID: owner.ID, Status: domain.StatusUp, Ping: 10 + k%20,
			Time: now.Add(-time.Duration(k) * 30 * time.Second),
		})
	}

	e := echo.New()
	e.GET("/api/monitors/:id/heartbeats/chart", h.GetChartData, func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(handlers.ContextUserIDKey, int64(1))
			return next(c)
		}
	})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/monitors/1/heartbeats/chart?hours=24", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("chart status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var view struct {
		Buckets []struct {
			Time string `json:"time"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if n := len(view.Buckets); n < 95 || n > 97 {
		t.Fatalf("chart has %d buckets; want ~96 covering 24 h", n)
	}
	first, err := time.Parse(time.RFC3339, view.Buckets[0].Time)
	if err != nil {
		t.Fatal(err)
	}
	if from := now.Add(-24 * time.Hour); first.After(from.Add(15 * time.Minute)) {
		t.Fatalf("first bucket %s; want the start of the 24 h window %s", first, from)
	}
}
