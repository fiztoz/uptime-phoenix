package handlers

import (
	"net/http"
	"reflect"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// requireNotificationRetarget writes a 403 and returns a non-nil error when
// the request changes the config (the destination) of a notification that
// the actor may not retarget: a non-admin may not change an admin-owned
// notification, whose deliveries reach private addresses on the admin's
// authority (#88). An unchanged config (a rename, a toggle) is always fine.
func (h *NotificationHandlers) requireNotificationRetarget(c echo.Context, actorID int64, existing *domain.Notification, newConfig map[string]any) error {
	if newConfig == nil || reflect.DeepEqual(newConfig, existing.Config) {
		return nil
	}
	if h.access == nil {
		_ = c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
		return errAccessDenied
	}
	ok, err := h.access.CanRetargetNotification(c.Request().Context(), actorID, existing.UserID)
	if err != nil {
		_ = mapNotifError(c, err)
		return errAccessDenied
	}
	if !ok {
		_ = c.JSON(http.StatusForbidden, errorBody("only an admin can change the destination of a notification created by an admin"))
		return errAccessDenied
	}
	return nil
}
