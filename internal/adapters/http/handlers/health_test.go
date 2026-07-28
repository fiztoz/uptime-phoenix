package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/version"
)

func serveHealth(t *testing.T, h *handlers.HealthHandlers, path string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.GET("/api/health/live", h.Live)
	e.GET("/api/health/ready", h.Ready)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// TestHealthHandlers_Live_IncludesVersion asserts the R6 contract: the
// liveness payload carries a "version" field read live from
// github.com/fiztoz/uptime-phoenix/internal/version.Version (the ldflags -X injection point), not a
// copied-at-init value.
func TestHealthHandlers_Live_IncludesVersion(t *testing.T) {
	orig := version.Version
	version.Version = "v9.9.9-test"
	defer func() { version.Version = orig }()

	rec := serveHealth(t, handlers.NewHealthHandlers(nil), "/api/health/live")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/health/live = %d; want 200", rec.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal live response: %v", err)
	}
	if body["status"] != "alive" {
		t.Errorf("status = %q; want alive", body["status"])
	}
	if body["version"] != "v9.9.9-test" {
		t.Errorf("version = %q; want the stamped v9.9.9-test (handler must read version.Version live)", body["version"])
	}
}

func TestHealthHandlers_Ready_OK(t *testing.T) {
	rec := serveHealth(t, handlers.NewHealthHandlers(func() bool { return true }), "/api/health/ready")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/health/ready = %d; want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal ready response: %v", err)
	}
	if body["status"] != "ready" {
		t.Errorf("status = %q; want ready", body["status"])
	}
}

func TestHealthHandlers_Ready_DatabaseDown(t *testing.T) {
	rec := serveHealth(t, handlers.NewHealthHandlers(func() bool { return false }), "/api/health/ready")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/health/ready with failing db = %d; want 503", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal ready response: %v", err)
	}
	if body["reason"] != "database unavailable" {
		t.Errorf("reason = %q; want database unavailable", body["reason"])
	}
}
