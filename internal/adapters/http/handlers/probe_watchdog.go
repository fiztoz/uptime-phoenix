package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// probeWatchdogSettingsStore is the saved per-probe connection watchdog
// settings, the same store phoenix-probe-admin watchdog writes.
type probeWatchdogSettingsStore interface {
	Get(ctx context.Context, probeID string) (domain.ProbeWatchdogSettings, error)
	Replace(ctx context.Context, probeID string, expectedRevision int64, desired domain.ProbeWatchdogSettings) (domain.ProbeWatchdogSettings, error)
}

// ProbeWatchdogHandlers serve GET/PUT /api/probes/:probe_id/watchdog, the
// HTTP equivalent of `phoenix-probe-admin watchdog` (GitHub #120).
type ProbeWatchdogHandlers struct {
	store   probeWatchdogSettingsStore
	enabled bool
}

// NewProbeWatchdogHandlers builds the handlers. A nil store answers 501.
func NewProbeWatchdogHandlers(store probeWatchdogSettingsStore, enabled bool) *ProbeWatchdogHandlers {
	return &ProbeWatchdogHandlers{store: store, enabled: enabled}
}

// ProbeWatchdogSettingsView is the wire shape of saved watchdog settings.
// Revision is a decimal string, like the other probe revisions.
type ProbeWatchdogSettingsView struct {
	Revision            string  `json:"revision"`
	Enabled             bool    `json:"enabled"`
	LostAfterSeconds    int32   `json:"lost_after_seconds"`
	RecoverAfterSeconds int32   `json:"recover_after_seconds"`
	ResendInterval      int32   `json:"resend_interval"`
	NotificationIDs     []int64 `json:"notification_ids"`
}

// probeWatchdogSettingsRequest replaces the complete settings. Enabled and
// expected_revision are required; omitted timings take the CLI defaults
// (90 s lost, 30 s recover, no reminders) and omitted channels mean none.
type probeWatchdogSettingsRequest struct {
	ExpectedRevision    *string `json:"expected_revision"`
	Enabled             *bool   `json:"enabled"`
	LostAfterSeconds    *int32  `json:"lost_after_seconds"`
	RecoverAfterSeconds *int32  `json:"recover_after_seconds"`
	ResendInterval      *int32  `json:"resend_interval"`
	NotificationIDs     []int64 `json:"notification_ids"`
}

func toProbeWatchdogSettingsView(s domain.ProbeWatchdogSettings) ProbeWatchdogSettingsView {
	ids := append([]int64{}, s.NotificationIDs...)
	return ProbeWatchdogSettingsView{
		Revision: strconv.FormatInt(s.Revision, 10), Enabled: s.Enabled,
		LostAfterSeconds: s.LostAfterSeconds, RecoverAfterSeconds: s.RecoverAfterSeconds,
		ResendInterval: s.ResendInterval, NotificationIDs: ids,
	}
}

func (h *ProbeWatchdogHandlers) prepare(c echo.Context) bool {
	c.Response().Header().Set("Cache-Control", "no-store")
	if userID, ok := userIDFromContext(c); !ok || userID <= 0 {
		_ = fleetError(c, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return false
	}
	if !h.enabled {
		_ = fleetError(c, http.StatusServiceUnavailable, "probes_disabled", "multi-region probes are disabled")
		return false
	}
	if h.store == nil {
		_ = fleetError(c, http.StatusNotImplemented, "watchdog_unavailable", "probe watchdog settings are not available on this hub")
		return false
	}
	return true
}

// Get handles GET /api/probes/:probe_id/watchdog.
func (h *ProbeWatchdogHandlers) Get(c echo.Context) error {
	if !h.prepare(c) {
		return nil
	}
	s, err := h.store.Get(c.Request().Context(), c.Param("probe_id"))
	if err != nil {
		return probeWatchdogError(c, err)
	}
	return c.JSON(http.StatusOK, toProbeWatchdogSettingsView(s))
}

// Replace handles PUT /api/probes/:probe_id/watchdog. Saving settings is not
// an applied-config receipt; the watchdog diagnostics on the probe detail
// view report when they take effect.
func (h *ProbeWatchdogHandlers) Replace(c echo.Context) error {
	if !h.prepare(c) {
		return nil
	}
	invalid := func() error {
		return fleetError(c, http.StatusBadRequest, "invalid_watchdog_settings",
			"watchdog settings need explicit enabled and expected_revision, positive lost/recover seconds, a non-negative resend interval and unique notification IDs")
	}
	raw, err := readProbeAdminBody(c)
	if err != nil {
		return invalid()
	}
	var req probeWatchdogSettingsRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Enabled == nil || req.ExpectedRevision == nil {
		return invalid()
	}
	expected, err := strconv.ParseInt(*req.ExpectedRevision, 10, 64)
	if err != nil || expected < 0 {
		return invalid()
	}
	desired := domain.DefaultProbeWatchdogSettings()
	desired.Enabled = *req.Enabled
	if req.LostAfterSeconds != nil {
		desired.LostAfterSeconds = *req.LostAfterSeconds
	}
	if req.RecoverAfterSeconds != nil {
		desired.RecoverAfterSeconds = *req.RecoverAfterSeconds
	}
	if req.ResendInterval != nil {
		desired.ResendInterval = *req.ResendInterval
	}
	if req.NotificationIDs != nil {
		desired.NotificationIDs = append([]int64{}, req.NotificationIDs...)
	}
	if domain.ValidateProbeWatchdogSettings(desired) != nil {
		return invalid()
	}
	saved, err := h.store.Replace(c.Request().Context(), c.Param("probe_id"), expected, desired)
	if err != nil {
		if errors.Is(err, domain.ErrValidation) {
			return invalid()
		}
		return probeWatchdogError(c, err)
	}
	return c.JSON(http.StatusOK, toProbeWatchdogSettingsView(saved))
}

func probeWatchdogError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, ports.ErrNotFound), errors.Is(err, domain.ErrNotFound):
		return fleetError(c, http.StatusNotFound, "probe_not_found", "probe not found")
	case errors.Is(err, ports.ErrConflict), errors.Is(err, domain.ErrConflict):
		return fleetError(c, http.StatusConflict, "watchdog_revision_conflict", "watchdog settings changed; reload and retry with the current revision")
	case errors.Is(err, domain.ErrValidation):
		return fleetError(c, http.StatusBadRequest, "invalid_watchdog_settings", "invalid watchdog settings")
	default:
		return fleetError(c, http.StatusServiceUnavailable, "fleet_unavailable", "fleet administration unavailable")
	}
}
