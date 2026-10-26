package handlers

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// saveBodyConfigKeys are the HTTP monitor options that echo response bodies
// into heartbeat messages and notifications.
var saveBodyConfigKeys = []string{"save_body_on_error", "save_body_on_success"}

// configEnablesSaveBody reports whether config turns on any save_body_* option.
func configEnablesSaveBody(config map[string]any) bool {
	for _, key := range saveBodyConfigKeys {
		if on, _ := config[key].(bool); on {
			return true
		}
	}
	return false
}

// monitorOutboundChange describes the outbound-sensitive parts of a monitor
// create, clone or update, for requireMonitorOutboundSettings.
type monitorOutboundChange struct {
	// ActorID is the user making the request.
	ActorID int64
	// OwnerID is the monitor's owner after the request (the creator for a
	// create or clone; the stored owner for an update).
	OwnerID int64
	// Config is the monitor config after the request, or nil when the request
	// does not change it.
	Config map[string]any
	// AssignsProxy is true when the request sets proxy_id to a value other
	// than the stored one (any non-null proxy_id on a create or clone).
	AssignsProxy bool
}

// requireMonitorOutboundSettings writes a 403 and returns a non-nil error when
// the request sets an option that widens what a monitor's check can reach or
// read back (#88), and the actor or owner may not:
//
//   - proxy_id: only an admin may assign a proxy, because the outbound guard
//     dials the proxy rather than the target;
//   - save_body_on_error / save_body_on_success: only on monitors owned by an
//     admin, because a saved body is shown to everyone who can view the
//     monitor.
//
// The decisions are AccessService's (CanAssignMonitorProxy,
// CanSaveResponseBody); the HTTP checker enforces the save_body rule again at
// check time.
func requireMonitorOutboundSettings(c echo.Context, access *services.AccessService, change monitorOutboundChange) error {
	if access == nil {
		_ = c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
		return errAccessDenied
	}
	ctx := c.Request().Context()
	if change.AssignsProxy {
		ok, err := access.CanAssignMonitorProxy(ctx, change.ActorID)
		if err != nil {
			_ = mapMonitorError(c, err)
			return errAccessDenied
		}
		if !ok {
			_ = c.JSON(http.StatusForbidden, errorBody("assigning a proxy to a monitor is limited to admins"))
			return errAccessDenied
		}
	}
	if change.Config != nil && configEnablesSaveBody(change.Config) && !access.CanSaveResponseBody(ctx, change.OwnerID) {
		_ = c.JSON(http.StatusForbidden, errorBody("save_body_on_error and save_body_on_success are limited to monitors owned by an admin"))
		return errAccessDenied
	}
	return nil
}
