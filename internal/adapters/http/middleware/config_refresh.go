package middleware

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ConfigRefreshAfterMutation wakes the local applied-configuration reconciler
// after successful source writes. It also reaches workers through the shared bus;
// periodic reconciliation covers lost events and non-HTTP writers.
func ConfigRefreshAfterMutation(bus ports.EventBus) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			method := c.Request().Method
			if err != nil || bus == nil || c.Response().Status < 200 || c.Response().Status >= 300 ||
				method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
				return err
			}
			path := c.Path()
			for _, prefix := range []string{
				"/api/monitors", "/api/monitor-groups", "/api/groups", "/api/notifications",
				"/api/notification-templates", "/api/tags", "/api/maintenance", "/api/proxies",
				"/api/escalation-policies", "/api/backup", "/api/settings",
			} {
				if path == prefix || strings.HasPrefix(path, prefix+"/") {
					if publishErr := bus.Publish(c.Request().Context(), ports.Event{Type: "config.refresh"}); publishErr != nil {
						slog.Error("configuration change notification failed; periodic refresh will retry")
					}
					break
				}
			}
			return nil
		}
	}
}
