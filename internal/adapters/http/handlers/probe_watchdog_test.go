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

	t.Run("omitted timings take the phoenix-probe-admin defaults", func(t *testing.T) {
		for _, tc := range []struct {
			name, body            string
			lost, recover, resend int32
		}{
			{"all omitted", `{"expected_revision":"0","enabled":true,"notification_ids":[3]}`, 90, 30, 0},
			{"only lost given", `{"expected_revision":"0","enabled":true,"notification_ids":[3],"lost_after_seconds":120}`, 120, 30, 0},
			{"only resend given", `{"expected_revision":"0","enabled":false,"notification_ids":[],"resend_interval":15}`, 90, 30, 15},
		} {
			t.Run(tc.name, func(t *testing.T) {
				store := &watchdogStoreFake{saved: domain.DefaultProbeWatchdogSettings()}
				rec := watchdogDo(watchdogRouter(store, true), http.MethodPut, "/api/probes/p1/watchdog", tc.body)
				if rec.Code != http.StatusOK {
					t.Fatalf("PUT status = %d; body=%s", rec.Code, rec.Body)
				}
				got := store.saved
				if got.LostAfterSeconds != tc.lost || got.RecoverAfterSeconds != tc.recover || got.ResendInterval != tc.resend {
					t.Fatalf("saved timings = %d/%d/%d; want %d/%d/%d", got.LostAfterSeconds, got.RecoverAfterSeconds, got.ResendInterval, tc.lost, tc.recover, tc.resend)
				}
			})
		}
		// The defaults come from the one shared source the CLI flags use.
		d := domain.DefaultProbeWatchdogSettings()
		if d.LostAfterSeconds != 90 || d.RecoverAfterSeconds != 30 || d.ResendInterval != 0 {
			t.Fatalf("shared defaults = %d/%d/%d; want 90/30/0", d.LostAfterSeconds, d.RecoverAfterSeconds, d.ResendInterval)
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
		{"enabled must be explicit", `{"expected_revision":"0","lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"enabled null is omitted", `{"expected_revision":"0","enabled":null,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"revision must be explicit", `{"enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"revision must be canonical decimal", `{"expected_revision":"00","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"lost seconds null is rejected", `{"expected_revision":"0","enabled":true,"lost_after_seconds":null,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"recover seconds null is rejected", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":null,"resend_interval":0,"notification_ids":[3]}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"resend interval null is rejected", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":null,"notification_ids":[3]}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"timing must be an integer", `{"expected_revision":"0","enabled":true,"lost_after_seconds":"90","notification_ids":[3]}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"timing must not be fractional", `{"expected_revision":"0","enabled":true,"recover_after_seconds":30.5,"notification_ids":[3]}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"unknown field with omitted timings", `{"expected_revision":"0","enabled":true,"notification_ids":[3],"lost_after":90}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"channels must be explicit", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"channels null is omitted", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":null}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"duplicate channels", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3,3]}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"non-positive timing", `{"expected_revision":"0","enabled":true,"lost_after_seconds":0,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"negative resend", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":-1,"notification_ids":[3]}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"unknown field", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3],"bogus":1}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"trailing JSON value", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}` + `{}`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"trailing garbage", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}` + `x`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"truncated JSON", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}`[:40], &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"not an object", `[]`, &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"oversized body", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}` + strings.Repeat(" ", 64<<10), &watchdogStoreFake{}, true, "/api/probes/p1/watchdog", 413, "watchdog_settings_too_large"},
		{"stale revision", strings.Replace(`{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}`, `"0"`, `"5"`, 1), &watchdogStoreFake{saved: domain.DefaultProbeWatchdogSettings()}, true, "/api/probes/p1/watchdog", 409, "watchdog_revision_conflict"},
		{"disabled probe", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}`, &watchdogStoreFake{saved: domain.DefaultProbeWatchdogSettings(), err: ports.ErrConflict}, true, "/api/probes/p1/watchdog", 409, "probe_disabled"},
		{"unknown channel", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}`, &watchdogStoreFake{err: domain.ErrValidation}, true, "/api/probes/p1/watchdog", 400, "invalid_watchdog_settings"},
		{"unknown probe", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}`, &watchdogStoreFake{err: ports.ErrNotFound}, true, "/api/probes/p1/watchdog", 404, "probe_not_found"},
		{"probes disabled", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}`, &watchdogStoreFake{}, false, "/api/probes/p1/watchdog", 503, "probes_disabled"},
		{"no store wired", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}`, nil, true, "/api/probes/p1/watchdog", 501, "watchdog_unavailable"},
		{"no store wired is not reported as probes disabled", `{"expected_revision":"0","enabled":true,"lost_after_seconds":90,"recover_after_seconds":30,"resend_interval":0,"notification_ids":[3]}`, nil, false, "/api/probes/p1/watchdog", 501, "watchdog_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := watchdogDo(watchdogRouter(tc.store, tc.enabled), http.MethodPut, tc.path, tc.body)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), `"`+tc.code+`"`) {
				t.Fatalf("status %d body %s; want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
			if (tc.status == 400 || tc.status == 413) && tc.store != nil && tc.store.err == nil && tc.store.calls != 0 {
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
