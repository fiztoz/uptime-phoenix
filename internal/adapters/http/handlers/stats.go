package handlers

import (
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// StatsHandlers exposes monitor statistics endpoints. Read-only, gated on view
// access to the monitor being asked about.
type StatsHandlers struct {
	svc    *services.MonitorStatsService
	access *services.AccessService
}

// NewStatsHandlers creates handlers for monitor statistics.
func NewStatsHandlers(svc *services.MonitorStatsService, access *services.AccessService) *StatsHandlers {
	return &StatsHandlers{svc: svc, access: access}
}

type monitorStatsView struct {
	CurrentPingMs  int      `json:"current_ping_ms"`
	AvgPing24h     float64  `json:"avg_ping_24h"`
	Uptime24h      *float64 `json:"uptime_24h"`
	Uptime30d      *float64 `json:"uptime_30d"`
	CertExpiryDate *string  `json:"cert_expiry_date"`
	CertDaysLeft   *int     `json:"cert_days_left"`
}

// GetStats handles GET /api/monitors/:id/stats.
func (h *StatsHandlers) GetStats(c echo.Context) error {
	monitorID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor id")
	}
	if err := requireMonitorViewAccess(c, h.access, monitorID); err != nil {
		return err
	}

	stats, err := h.svc.GetStats(c.Request().Context(), monitorID)
	if err != nil {
		return mapMonitorError(c, err)
	}

	return c.JSON(http.StatusOK, monitorStatsView{
		CurrentPingMs:  stats.CurrentPingMs,
		AvgPing24h:     stats.AvgPing24h,
		Uptime24h:      stats.Uptime24h,
		Uptime30d:      stats.Uptime30d,
		CertExpiryDate: stats.CertExpiryDate,
		CertDaysLeft:   stats.CertDaysLeft,
	})
}
