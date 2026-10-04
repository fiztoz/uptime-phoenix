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

type overallHistoryFake struct {
	result *services.OverallHistoryResult
	err    error
	calls  int
}

func (f *overallHistoryFake) OverallHistory(_ context.Context, _, _ int64, _, _ time.Time) (*services.OverallHistoryResult, error) {
	f.calls++
	return f.result, f.err
}

func overallHarness(t *testing.T, overall overallHistoryFake, rows []*domain.Heartbeat) *echo.Echo {
	t.Helper()
	monRepo := newFakeMonitorRepo()
	owner := &domain.Monitor{UserID: 1, Name: "m", Type: "http", Active: true, Interval: 60}
	if err := monRepo.Create(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	hbRepo := &hbWindowRepo{heartbeats: rows}
	hbSvc := services.NewHeartbeatService(hbRepo, newFakeMonitorBus())
	userRepo := memory.NewUserRepo()
	_ = userRepo.Create(context.Background(), &domain.User{Username: "admin", Active: true, IsAdmin: true})
	accessSvc := services.NewAccessService(userRepo, memory.NewUserPermissionRepo(), nil, monRepo)

	h := handlers.NewHeartbeatHandlers(hbSvc, accessSvc)
	h.SetOverall(&overall)
	e := echo.New()
	e.HideBanner, e.HidePort = true, true
	g := e.Group("/api/monitors/:id/heartbeats", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(handlers.ContextUserIDKey, int64(1))
			return next(c)
		}
	})
	g.GET("", h.ListByMonitor)
	g.GET("/chart", h.GetChartData)
	return e
}

// TestHeartbeatOverallStreamActivation pins the section-7.2 compatibility
// change for multi-probe monitors: the unqualified endpoints serve the
// policy-evaluated overall timeline with scope "overall", latency_available
// false and the unmeasured zero ping sentinel. UNKNOWN stays visible and the
// overall chart carries no synthetic latency buckets.
func TestHeartbeatOverallStreamActivation(t *testing.T) {
	base := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	intervals := []domain.MonitorHealthInterval{
		{From: base, To: base.Add(30 * time.Minute), Status: domain.StatusUp},
		{From: base.Add(30 * time.Minute), To: base.Add(45 * time.Minute), Status: domain.StatusDown, Reason: "target_down"},
		{From: base.Add(45 * time.Minute), To: base.Add(60 * time.Minute), Status: domain.StatusDown, Reason: "target_down"},
		{From: base.Add(60 * time.Minute), To: base.Add(90 * time.Minute), Status: domain.StatusUnknown, Reason: "missing_snapshot_state"},
	}
	rows := []services.OverallHistoryRow{
		{From: base, Status: domain.StatusUp, Important: true},
		{From: base.Add(30 * time.Minute), Status: domain.StatusDown, Reason: "target_down", Important: true},
		{From: base.Add(45 * time.Minute), Status: domain.StatusDown, Reason: "target_down", Important: false},
		{From: base.Add(60 * time.Minute), Status: domain.StatusUnknown, Reason: "missing_snapshot_state", Important: true},
	}
	e := overallHarness(t, overallHistoryFake{result: &services.OverallHistoryResult{Rows: rows, Intervals: intervals}}, nil)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/monitors/1/heartbeats?order=asc", nil))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 overall segments, got %d: %s", len(got), rec.Body.String())
	}
	for i, row := range got {
		if row["scope"] != "overall" || row["latency_available"] != false {
			t.Fatalf("row %d lost its section-7.2 markers: %v", i, row)
		}
		if row["ping"] != float64(0) {
			t.Fatalf("overall ping must be the unmeasured zero sentinel: %v", row)
		}
		if row["message"] == nil {
			t.Fatalf("overall rows must carry their bounded reason: %v", row)
		}
	}
	if got[3]["status"] != "unknown" {
		t.Fatalf("UNKNOWN must stay visible: %v", got[3])
	}
	if got[1]["id"] == got[2]["id"] {
		t.Fatalf("window sequence ids must be unique: %v", got)
	}

	// important=true keeps only status-change segments.
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/monitors/1/heartbeats?important=true&order=asc", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("important filter: %s", rec.Body.String())
	}

	// The overall chart has no synthetic latency buckets and merges consecutive
	// same-kind segments into single runs.
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/monitors/1/heartbeats/chart", nil))
	var chart struct {
		Buckets           []map[string]any `json:"buckets"`
		DowntimeIntervals []map[string]any `json:"downtime_intervals"`
		UnknownIntervals  []map[string]any `json:"unknown_intervals"`
		Scope             string           `json:"scope"`
		LatencyAvailable  bool             `json:"latency_available"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chart); err != nil {
		t.Fatal(err)
	}
	if chart.Scope != "overall" || chart.LatencyAvailable {
		t.Fatalf("overall chart markers: %s", rec.Body.String())
	}
	if len(chart.Buckets) != 0 {
		t.Fatalf("overall chart must not synthesize latency buckets: %s", rec.Body.String())
	}
	if len(chart.DowntimeIntervals) != 1 || len(chart.UnknownIntervals) != 1 {
		t.Fatalf("downtime/unknown runs must merge and stay visible: %s", rec.Body.String())
	}
}

// TestHeartbeatLocalStreamKeepsMeasuredLatency proves the local-only contract:
// today's measured rows and buckets are preserved and only gain markers.
func TestHeartbeatLocalStreamKeepsMeasuredLatency(t *testing.T) {
	rows := []*domain.Heartbeat{{
		ID: 7, MonitorID: 1, Status: domain.StatusUp, Ping: 42,
		Time: time.Now().UTC().Add(-30 * time.Second),
	}}
	e := overallHarness(t, overallHistoryFake{result: &services.OverallHistoryResult{LocalOnly: true}}, rows)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/monitors/1/heartbeats", nil))
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("%s", rec.Body.String())
	}
	row := got[0]
	if row["scope"] != "local" || row["latency_available"] != true {
		t.Fatalf("local markers: %v", row)
	}
	if row["ping"] != float64(42) || row["id"] != float64(7) {
		t.Fatalf("local measured rows must be preserved: %v", row)
	}

	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/monitors/1/heartbeats/chart?hours=1", nil))
	var chart struct {
		Buckets          []map[string]any `json:"buckets"`
		Scope            string           `json:"scope"`
		LatencyAvailable bool             `json:"latency_available"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chart); err != nil {
		t.Fatal(err)
	}
	if chart.Scope != "local" || !chart.LatencyAvailable {
		t.Fatalf("local chart markers: %s", rec.Body.String())
	}
	if len(chart.Buckets) == 0 {
		t.Fatalf("local chart keeps today's measured buckets: %s", rec.Body.String())
	}
}
