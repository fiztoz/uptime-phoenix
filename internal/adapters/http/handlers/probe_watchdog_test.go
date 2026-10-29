package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type watchdogStoreFake struct {
	saved    domain.ProbeWatchdogSettings
	expected int64
	calls    int
	err      error
}

func (f *watchdogStoreFake) Get(_ context.Context, probeID string) (domain.ProbeWatchdogSettings, error) {
	if probeID != "p1" {
		return domain.ProbeWatchdogSettings{}, ports.ErrNotFound
	}
	return f.saved, nil
}

func (f *watchdogStoreFake) Replace(_ context.Context, probeID string, expected int64, desired domain.ProbeWatchdogSettings) (domain.ProbeWatchdogSettings, error) {
	f.calls++
	if f.err != nil {
		return domain.ProbeWatchdogSettings{}, f.err
	}
	if expected != f.saved.Revision {
		return domain.ProbeWatchdogSettings{}, ports.ErrConflict
	}
	f.expected = expected
	desired.Revision = expected + 1
	f.saved = desired
	return desired, nil
}

func watchdogRouter(store *watchdogStoreFake, enabled bool) *echo.Echo {
	e := echo.New()
	var h *handlers.ProbeWatchdogHandlers
	if store == nil {
		h = handlers.NewProbeWatchdogHandlers(nil, enabled)
	} else {
		h = handlers.NewProbeWatchdogHandlers(store, enabled)
	}
	withUser := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error { c.Set(handlers.ContextUserIDKey, int64(1)); return next(c) }
	}
	e.GET("/api/probes/:probe_id/watchdog", h.Get, withUser)
	e.PUT("/api/probes/:probe_id/watchdog", h.Replace, withUser)
	return e
}

func watchdogDo(e *echo.Echo, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// TestProbeWatchdogSettingsOverHTTP pins #120: probe-offline alerting could
// only be configured with phoenix-probe-admin, so probes enrolled through the
// API/UI had no way to turn it on.
func TestProbeWatchdogSettingsOverHTTP(t *testing.T) {
	t.Run("get returns the saved settings", func(t *testing.T) {
		store := &watchdogStoreFake{saved: domain.DefaultProbeWatchdogSettings()}
		rec := watchdogDo(watchdogRouter(store, true), http.MethodGet, "/api/probes/p1/watchdog", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET status = %d; body=%s", rec.Code, rec.Body)
		}
		var view handlers.ProbeWatchdogSettingsView
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if view.Enabled || view.Revision != "0" || view.LostAfterSeconds != 90 || view.RecoverAfterSeconds != 30 || view.NotificationIDs == nil {
			t.Fatalf("view = %+v; want the opt-in defaults at revision 0", view)
		}
	})

	t.Run("put enables offline alerting", func(t *testing.T) {
		store := &watchdogStoreFake{saved: domain.DefaultProbeWatchdogSettings()}
		rec := watchdogDo(watchdogRouter(store, true), http.MethodPut, "/api/probes/p1/watchdog",
			`{"expected_revision":"0","enabled":true,"notification_ids":[3,7],"lost_after_seconds":120,"recover_after_seconds":45,"resend_interval":15}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT status = %d; body=%s", rec.Code, rec.Body)
		}
		got := store.saved
		if !got.Enabled || got.LostAfterSeconds != 120 || got.RecoverAfterSeconds != 45 || got.ResendInterval != 15 || !slices.Equal(got.NotificationIDs, []int64{3, 7}) {
			t.Fatalf("saved = %+v; want enabled with channels 3,7 and the given timings", got)
		}
		var view handlers.ProbeWatchdogSettingsView
		_ = json.Unmarshal(rec.Body.Bytes(), &view)
		if view.Revision != "1" || !view.Enabled {
			t.Fatalf("response = %+v; want revision 1, enabled", view)
		}
	})

	t.Run("put defaults timings like the CLI", func(t *testing.T) {
		store := &watchdogStoreFake{saved: domain.DefaultProbeWatchdogSettings()}
		rec := watchdogDo(watchdogRouter(store, true), http.MethodPut, "/api/probes/p1/watchdog",
			`{"expected_revision":"0","enabled":true,"notification_ids":[3]}`)
		if rec.Code != http.StatusOK || store.saved.LostAfterSeconds != 90 || store.saved.RecoverAfterSeconds != 30 || store.saved.ResendInterval != 0 {
			t.Fatalf("status %d saved %+v; want 90/30/0 defaults", rec.Code, store.saved)
		}
	})

	for _, tc := range []struct {
		name, body string
		store      *watchdogStoreFake
		enabled    bool
		path       string
		status     int
		code       string
	}{
		{"enabled must be explicit", `{"expected_revision":"0","notification_ids":[3]}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"revision must be explicit", `{"enabled":true}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"duplicate channels", `{"expected_revision":"0","enabled":true,"notification_ids":[3,3]}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"non-positive timing", `{"expected_revision":"0","enabled":true,"lost_after_seconds":0}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"unknown field", `{"expected_revision":"0","enabled":true,"bogus":1}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"stale revision", `{"expected_revision":"5","enabled":true}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 409, "watchdog_revision_conflict"},
		{"unknown channel or disabled probe", `{"expected_revision":"0","enabled":true,"notification_ids":[99]}`, &watchdogStoreFake{err: domain.ErrValidation}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"probes disabled", `{"expected_revision":"0","enabled":true}`, &watchdogStoreFake{}, false, "/api/probes/p1/watchdog", 503, "probes_disabled"},
		{"no store wired", `{"expected_revision":"0","enabled":true}`, nil, true, "/api/probes/p1/watchdog", 501, "watchdog_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := watchdogDo(watchdogRouter(tc.store, tc.enabled), http.MethodPut, tc.path, tc.body)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), `"`+tc.code+`"`) {
				t.Fatalf("status %d body %s; want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
			if tc.status == 400 && tc.store != nil && tc.store.err == nil && tc.store.calls != 0 {
				t.Fatalf("invalid body reached the store")
			}
		})
	}

	t.Run("get unknown probe is 404", func(t *testing.T) {
		rec := watchdogDo(watchdogRouter(&watchdogStoreFake{}, true), http.MethodGet, "/api/probes/nope/watchdog", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d; want 404", rec.Code)
		}
	})
}
