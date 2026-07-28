package handlers

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/version"
)

// HealthHandlers holds handlers for health check endpoints.
type HealthHandlers struct {
	dbChecker func() bool
}

// NewHealthHandlers creates health check handlers.
// dbChecker is called to verify database connectivity.
func NewHealthHandlers(dbChecker func() bool) *HealthHandlers {
	return &HealthHandlers{dbChecker: dbChecker}
}

// Live returns 200 if the process is alive (liveness probe). The payload
// carries the build version ("dev" when unstamped — see internal/version)
// so the UI footer and operators can read what is running.
func (h *HealthHandlers) Live(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{
		"status":  "alive",
		"version": version.Version,
	})
}

// Ready returns 200 if the application is ready to serve traffic (readiness probe).
// Checks database connectivity.
func (h *HealthHandlers) Ready(c echo.Context) error {
	if h.dbChecker != nil && !h.dbChecker() {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"status": "not ready",
			"reason": "database unavailable",
		})
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "ready"})
}
