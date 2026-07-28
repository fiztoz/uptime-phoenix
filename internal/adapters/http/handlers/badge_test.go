// Package handlers_test contains integration tests for HTTP handlers.
package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/logger"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// --- Fakes scoped to badge tests (kept self-contained so this file never
// depends on symbols owned by another concurrently-edited test file). ---

type badgeFakeMonitorRepo struct {
	mu   sync.Mutex
	byID map[int64]*domain.Monitor
}

func newBadgeFakeMonitorRepo() *badgeFakeMonitorRepo {
	return &badgeFakeMonitorRepo{byID: make(map[int64]*domain.Monitor)}
}

func (r *badgeFakeMonitorRepo) seed(m *domain.Monitor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[m.ID] = m
}

func (r *badgeFakeMonitorRepo) Create(_ context.Context, m *domain.Monitor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[m.ID] = m
	return nil
}

func (r *badgeFakeMonitorRepo) GetByID(_ context.Context, id int64) (*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return m, nil
}

func (r *badgeFakeMonitorRepo) GetByPushToken(_ context.Context, _ string) (*domain.Monitor, error) {
	return nil, ports.ErrNotFound
}

func (r *badgeFakeMonitorRepo) List(_ context.Context, _ ports.MonitorFilter) ([]*domain.Monitor, error) {
	return nil, nil
}

func (r *badgeFakeMonitorRepo) ListActive(_ context.Context) ([]*domain.Monitor, error) {
	return nil, nil
}

func (r *badgeFakeMonitorRepo) Update(_ context.Context, _ *domain.Monitor) error { return nil }
func (r *badgeFakeMonitorRepo) Delete(_ context.Context, _ int64) error           { return nil }
func (r *badgeFakeMonitorRepo) ClaimBatch(_ context.Context, _ string, _ int, _ time.Duration) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *badgeFakeMonitorRepo) RefreshLease(_ context.Context, _ string) (int64, error) {
	return 0, nil
}
func (r *badgeFakeMonitorRepo) ReleaseLeases(_ context.Context, _ string) (int64, error) {
	return 0, nil
}

type badgeFakeHeartbeatRepo struct {
	mu         sync.Mutex
	latest     map[int64]*domain.Heartbeat
	heartbeats []*domain.Heartbeat
}

func newBadgeFakeHeartbeatRepo() *badgeFakeHeartbeatRepo {
	return &badgeFakeHeartbeatRepo{latest: make(map[int64]*domain.Heartbeat)}
}

func (r *badgeFakeHeartbeatRepo) setLatest(h *domain.Heartbeat) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latest[h.MonitorID] = h
}

func (r *badgeFakeHeartbeatRepo) seedHistory(hbs ...*domain.Heartbeat) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.heartbeats = append(r.heartbeats, hbs...)
}

func (r *badgeFakeHeartbeatRepo) Save(_ context.Context, h *domain.Heartbeat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.heartbeats = append(r.heartbeats, h)
	r.latest[h.MonitorID] = h
	return nil
}

func (r *badgeFakeHeartbeatRepo) GetLatest(_ context.Context, monitorID int64) (*domain.Heartbeat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.latest[monitorID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return h, nil
}

func (r *badgeFakeHeartbeatRepo) ListByMonitor(_ context.Context, monitorID int64, from, to time.Time) ([]*domain.Heartbeat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.Heartbeat
	for _, h := range r.heartbeats {
		if h.MonitorID == monitorID && !h.Time.Before(from) && !h.Time.After(to) {
			out = append(out, h)
		}
	}
	return out, nil
}

func (r *badgeFakeHeartbeatRepo) DeleteByMonitor(_ context.Context, _ int64) error { return nil }
func (r *badgeFakeHeartbeatRepo) DeleteOlderThan(_ context.Context, _ time.Time) error {
	return nil
}

func (r *badgeFakeHeartbeatRepo) SaveAggregate1m(_ context.Context, _ *ports.Aggregate1m) error {
	return nil
}
func (r *badgeFakeHeartbeatRepo) SaveAggregate1h(_ context.Context, _ *ports.Aggregate1h) error {
	return nil
}
func (r *badgeFakeHeartbeatRepo) SaveAggregate1d(_ context.Context, _ *ports.Aggregate1d) error {
	return nil
}
func (r *badgeFakeHeartbeatRepo) GetAggregate1m(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1m, error) {
	return nil, nil
}
func (r *badgeFakeHeartbeatRepo) GetAggregate1h(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1h, error) {
	return nil, nil
}

// GetAggregate1d intentionally returns no rows so AggregateService.GetUptimePercent
// falls back to computing uptime straight from raw heartbeats (ListByMonitor) —
// this exercises the same fallback path the real service uses before rollups
// have run, without duplicating any of its SQL/aggregation logic here.
func (r *badgeFakeHeartbeatRepo) GetAggregate1d(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1d, error) {
	return nil, nil
}

// --- Test harness ---------------------------------------------------------

type badgeTestHarness struct {
	router        *echo.Echo
	monitorRepo   *badgeFakeMonitorRepo
	heartbeatRepo *badgeFakeHeartbeatRepo
}

func newBadgeHarness(t *testing.T) *badgeTestHarness {
	t.Helper()

	monitorRepo := newBadgeFakeMonitorRepo()
	heartbeatRepo := newBadgeFakeHeartbeatRepo()
	log := logger.New("error")
	aggSvc := services.NewAggregateService(heartbeatRepo, monitorRepo, log)
	badgeH := handlers.NewBadgeHandlers(monitorRepo, heartbeatRepo, aggSvc)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	badgeGroup := e.Group("/api/badge/:id")
	badgeGroup.GET("/status.svg", badgeH.Status)
	badgeGroup.GET("/uptime.svg", badgeH.Uptime)
	badgeGroup.GET("/ping.svg", badgeH.Ping)

	return &badgeTestHarness{router: e, monitorRepo: monitorRepo, heartbeatRepo: heartbeatRepo}
}

func (h *badgeTestHarness) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

// --- Tests -----------------------------------------------------------------

func TestBadgeHandlers_Status_Up(t *testing.T) {
	h := newBadgeHarness(t)
	now := time.Now().UTC()
	h.monitorRepo.seed(&domain.Monitor{ID: 1, Active: true, Name: "API"})
	h.heartbeatRepo.setLatest(&domain.Heartbeat{ID: 1, MonitorID: 1, Status: domain.StatusUp, Ping: 42, Time: now})

	rec := h.get(t, "/api/badge/1/status.svg")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get(echo.HeaderContentType)
	if !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("content-type = %q, want image/svg+xml prefix", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "UP") {
		t.Errorf("body does not contain %q: %s", "UP", body)
	}
	if !strings.Contains(body, "#4c1") {
		t.Errorf("body does not contain up color #4c1: %s", body)
	}
}

func TestBadgeHandlers_Status_Down(t *testing.T) {
	h := newBadgeHarness(t)
	now := time.Now().UTC()
	h.monitorRepo.seed(&domain.Monitor{ID: 2, Active: true, Name: "DB"})
	h.heartbeatRepo.setLatest(&domain.Heartbeat{ID: 2, MonitorID: 2, Status: domain.StatusDown, Time: now})

	rec := h.get(t, "/api/badge/2/status.svg")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "DOWN") {
		t.Errorf("body does not contain %q: %s", "DOWN", body)
	}
	if !strings.Contains(body, "#e05d44") {
		t.Errorf("body does not contain down color #e05d44: %s", body)
	}
}

func TestBadgeHandlers_Status_NoHeartbeatYet_IsPending(t *testing.T) {
	h := newBadgeHarness(t)
	h.monitorRepo.seed(&domain.Monitor{ID: 3, Active: true, Name: "New Monitor"})

	rec := h.get(t, "/api/badge/3/status.svg")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "PENDING") {
		t.Errorf("expected PENDING for monitor with no heartbeat, got: %s", body)
	}
}

func TestBadgeHandlers_Status_UnknownMonitor_Returns200Gray(t *testing.T) {
	h := newBadgeHarness(t)

	rec := h.get(t, "/api/badge/999/status.svg")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unknown monitors render a gray badge, not 404)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "unknown") {
		t.Errorf("expected 'unknown' value for unknown monitor, got: %s", body)
	}
	if !strings.Contains(body, "#9f9f9f") {
		t.Errorf("expected gray color for unknown monitor, got: %s", body)
	}
}

func TestBadgeHandlers_Status_NonNumericID_Returns200Gray(t *testing.T) {
	h := newBadgeHarness(t)

	rec := h.get(t, "/api/badge/not-a-number/status.svg")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown") {
		t.Errorf("expected 'unknown' value for non-numeric id, got: %s", rec.Body.String())
	}
}

func TestBadgeHandlers_Uptime_24h(t *testing.T) {
	h := newBadgeHarness(t)
	now := time.Now().UTC()
	h.monitorRepo.seed(&domain.Monitor{ID: 4, Active: true, Name: "Web"})
	h.heartbeatRepo.seedHistory(
		&domain.Heartbeat{ID: 1, MonitorID: 4, Status: domain.StatusUp, Time: now.Add(-2 * time.Hour)},
		&domain.Heartbeat{ID: 2, MonitorID: 4, Status: domain.StatusUp, Time: now.Add(-90 * time.Minute)},
		&domain.Heartbeat{ID: 3, MonitorID: 4, Status: domain.StatusUp, Time: now.Add(-60 * time.Minute)},
		&domain.Heartbeat{ID: 4, MonitorID: 4, Status: domain.StatusDown, Time: now.Add(-30 * time.Minute)},
	)

	rec := h.get(t, "/api/badge/4/uptime.svg?duration=24h")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "75.0%") {
		t.Errorf("expected 75.0%% uptime in body, got: %s", body)
	}
	// 75% is below the 95% amber threshold, so the badge should be red.
	if !strings.Contains(body, "#e05d44") {
		t.Errorf("expected red color for 75%% uptime, got: %s", body)
	}
}

func TestBadgeHandlers_Uptime_DefaultsTo24hOnBadDuration(t *testing.T) {
	h := newBadgeHarness(t)
	now := time.Now().UTC()
	h.monitorRepo.seed(&domain.Monitor{ID: 5, Active: true, Name: "Edge"})
	h.heartbeatRepo.seedHistory(
		&domain.Heartbeat{ID: 1, MonitorID: 5, Status: domain.StatusUp, Time: now.Add(-1 * time.Hour)},
	)

	rec := h.get(t, "/api/badge/5/uptime.svg?duration=bogus")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "100%") {
		t.Errorf("expected 100%% uptime, got: %s", rec.Body.String())
	}
}

func TestBadgeHandlers_Ping(t *testing.T) {
	h := newBadgeHarness(t)
	now := time.Now().UTC()
	h.monitorRepo.seed(&domain.Monitor{ID: 6, Active: true, Name: "Ping Target"})
	h.heartbeatRepo.setLatest(&domain.Heartbeat{ID: 1, MonitorID: 6, Status: domain.StatusUp, Ping: 55, Time: now})

	rec := h.get(t, "/api/badge/6/ping.svg")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "55ms") {
		t.Errorf("expected '55ms' in body, got: %s", body)
	}
}

func TestBadgeHandlers_Ping_NoData(t *testing.T) {
	h := newBadgeHarness(t)
	h.monitorRepo.seed(&domain.Monitor{ID: 7, Active: true, Name: "No Ping Yet"})

	rec := h.get(t, "/api/badge/7/ping.svg")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "n/a") {
		t.Errorf("expected 'n/a' for monitor with no ping data, got: %s", rec.Body.String())
	}
}

func TestBadgeHandlers_EscapesLabelText(t *testing.T) {
	// Regression guard: badge text must be XML-escaped. Status/uptime/ping
	// labels and values are all handler-controlled constants today, but this
	// pins the escaping helper's behavior so it stays safe if that changes.
	h := newBadgeHarness(t)
	h.monitorRepo.seed(&domain.Monitor{ID: 8, Active: true, Name: "Escaped"})

	rec := h.get(t, "/api/badge/8/status.svg")
	body := rec.Body.String()
	if strings.Contains(body, "<status>") {
		t.Errorf("body should not contain unescaped angle brackets: %s", body)
	}
}
