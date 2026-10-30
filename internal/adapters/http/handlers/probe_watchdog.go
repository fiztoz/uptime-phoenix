package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

// probeWatchdogSettingsRequest replaces the complete settings. Every field is
// required, so a client cannot reset timings or channels by omission, and the
// channel list is always an explicit choice (an empty list means none). Null
// counts as omitted.
type probeWatchdogSettingsRequest struct {
	ExpectedRevision    *string  `json:"expected_revision"`
	Enabled             *bool    `json:"enabled"`
	LostAfterSeconds    *int32   `json:"lost_after_seconds"`
	RecoverAfterSeconds *int32   `json:"recover_after_seconds"`
	ResendInterval      *int32   `json:"resend_interval"`
	NotificationIDs     *[]int64 `json:"notification_ids"`
}

// probeWatchdogMaxBodyBytes bounds a settings document. The domain allows at
// most 1000 channel IDs, which fits well inside it.
const probeWatchdogMaxBodyBytes = 64 << 10

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
	// A hub without the store (including the router fallback) has not wired
	// this endpoint. That says nothing about whether probes are enabled, so
	// it answers 501 before the feature check.
	if h.store == nil {
		_ = fleetError(c, http.StatusNotImplemented, "watchdog_unavailable", "probe watchdog settings are not available on this hub")
		return false
	}
	if !h.enabled {
		_ = fleetError(c, http.StatusServiceUnavailable, "probes_disabled", "multi-region probes are disabled")
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

// Replace handles PUT /api/probes/:probe_id/watchdog. It saves the desired
// settings only. Saving is not an applied-config receipt: the probe config
// pipeline prepares a new revision from the saved settings, and the watchdog
// diagnostics on the probe detail view report when the probe applies it.
func (h *ProbeWatchdogHandlers) Replace(c echo.Context) error {
	if !h.prepare(c) {
		return nil
	}
	invalid := func() error {
		return fleetError(c, http.StatusBadRequest, "invalid_watchdog_settings",
			"watchdog settings need one JSON object with expected_revision, enabled, lost_after_seconds, recover_after_seconds, resend_interval and notification_ids; positive lost/recover seconds, a non-negative resend interval, and unique existing notification IDs")
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, probeWatchdogMaxBodyBytes+1))
	if err != nil {
		return invalid()
	}
	if len(raw) > probeWatchdogMaxBodyBytes {
		return fleetError(c, http.StatusRequestEntityTooLarge, "watchdog_settings_too_large", "request body exceeds the watchdog settings limit")
	}
	desired, expected, ok := decodeProbeWatchdogSettings(raw)
	if !ok {
		return invalid()
	}
	probeID := c.Param("probe_id")
	saved, err := h.store.Replace(c.Request().Context(), probeID, expected, desired)
	if err != nil {
		if errors.Is(err, domain.ErrValidation) {
			return invalid()
		}
		if errors.Is(err, ports.ErrConflict) || errors.Is(err, domain.ErrConflict) {
			// The store also answers conflict for a disabled probe. Tell the
			// caller which one it was, so a disabled probe is not reported as
			// a stale revision.
			if current, getErr := h.store.Get(c.Request().Context(), probeID); getErr == nil && current.Revision == expected {
				return fleetError(c, http.StatusConflict, "probe_disabled", "the probe is disabled; enable it before changing its watchdog settings")
			}
		}
		return probeWatchdogError(c, err)
	}
	return c.JSON(http.StatusOK, toProbeWatchdogSettingsView(saved))
}

// decodeProbeWatchdogSettings accepts exactly one JSON object with every
// field present and non-null, no unknown fields, and nothing after it.
func decodeProbeWatchdogSettings(raw []byte) (domain.ProbeWatchdogSettings, int64, bool) {
	var req probeWatchdogSettingsRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return domain.ProbeWatchdogSettings{}, 0, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return domain.ProbeWatchdogSettings{}, 0, false
	}
	if req.ExpectedRevision == nil || req.Enabled == nil || req.LostAfterSeconds == nil ||
		req.RecoverAfterSeconds == nil || req.ResendInterval == nil || req.NotificationIDs == nil {
		return domain.ProbeWatchdogSettings{}, 0, false
	}
	expected, err := strconv.ParseInt(*req.ExpectedRevision, 10, 64)
	if err != nil || expected < 0 || strconv.FormatInt(expected, 10) != *req.ExpectedRevision {
		return domain.ProbeWatchdogSettings{}, 0, false
	}
	desired := domain.ProbeWatchdogSettings{
		Enabled: *req.Enabled, LostAfterSeconds: *req.LostAfterSeconds,
		RecoverAfterSeconds: *req.RecoverAfterSeconds, ResendInterval: *req.ResendInterval,
		NotificationIDs: append([]int64{}, (*req.NotificationIDs)...),
	}
	if domain.ValidateProbeWatchdogSettings(desired) != nil {
		return domain.ProbeWatchdogSettings{}, 0, false
	}
	return desired, expected, true
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
