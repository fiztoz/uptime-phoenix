package middleware

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// Capability names one of the optional write powers a non-admin user can hold.
// Admins implicitly hold every capability — see services.AccessService.
type Capability string

const (
	// CapManageNotifications allows creating/updating/deleting notifications and
	// attaching them to monitors the user can view.
	CapManageNotifications Capability = "manage_notifications"
	// CapManageMaintenance allows creating/updating/deleting maintenance windows
	// and assigning monitors the user can view to them.
	CapManageMaintenance Capability = "manage_maintenance"
	// CapCreateMonitors allows creating monitors. It does NOT allow touching a
	// monitor somebody else created — that is decided per-resource by ownership,
	// which middleware cannot check because it does not know the target's owner.
	// Routes that mutate an EXISTING monitor must gate in the handler with
	// requireMonitorEditAccess instead of reaching for this.
	CapCreateMonitors Capability = "create_monitors"
	// CapCreateGroups allows creating monitor groups (folders). Same boundary as
	// CapCreateMonitors: creation only, never edits to an existing group.
	CapCreateGroups Capability = "create_groups"
)

// RequireCapability returns middleware that restricts a route to users holding a
// capability (or to admins, who hold all of them).
//
// It mirrors RequireAdmin: it must run AFTER a middleware that has already
// authenticated the request and stored the resolved user id under
// ContextUserIDKey — this performs authorization only, never authentication.
//
// It fails closed on every abnormal path: no user id → 401; nil access service or
// a failed capability lookup → 403. There is no branch here that lets a request
// through because something went wrong.
func RequireCapability(access *services.AccessService, capability Capability) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			userID, ok := contextUserID(c)
			if !ok {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			}
			if access == nil {
				return c.JSON(http.StatusForbidden, map[string]string{"error": "access control unavailable"})
			}

			var allowed bool
			var err error
			switch capability {
			case CapManageNotifications:
				allowed, err = access.CanManageNotifications(c.Request().Context(), userID)
			case CapManageMaintenance:
				allowed, err = access.CanManageMaintenance(c.Request().Context(), userID)
			case CapCreateMonitors:
				allowed, err = access.CanCreateMonitors(c.Request().Context(), userID)
			case CapCreateGroups:
				allowed, err = access.CanCreateGroups(c.Request().Context(), userID)
			default:
				// An unknown capability is a programming error, and the only safe
				// reading of "I don't know what this permission is" is "no".
				allowed = false
			}
			if err != nil || !allowed {
				return c.JSON(http.StatusForbidden, map[string]string{"error": "insufficient permissions"})
			}
			return next(c)
		}
	}
}
