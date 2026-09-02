package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// errNotificationNotFound is the client-facing message used whenever a
// notification does not exist or the caller may not see it. Reusing one message
// avoids leaking whether the id exists (same pattern as errMaintenanceNotFound).
const errNotificationNotFound = "notification not found"

// NotificationHandlers groups the notification CRUD endpoints and
// monitor-assignment endpoints behind a single receiver.
//
// RBAC. Notifications are install-wide objects, not per-owner ones:
//
//   - Admins and users holding can_manage_notifications may create, update,
//     delete and test ANY notification, whoever created it. A capability holder who
//     could not touch the notifications the admin created would hold a useless
//     grant, so ownership is deliberately NOT a gate on this resource.
//   - Everyone else gets a read-only view restricted to the notifications attached
//     to monitors they can see.
//
// The capability gate on the mutating routes is applied by
// middleware.RequireCapability in the router; the checks in these handlers are the
// second layer. The read paths do their own scoping, because middleware can reject
// a request but cannot narrow a result set.
type NotificationHandlers struct {
	svc    *services.NotificationService
	access *services.AccessService
}

// NewNotificationHandlers creates handlers bound to the supplied services.
func NewNotificationHandlers(svc *services.NotificationService, access *services.AccessService) *NotificationHandlers {
	return &NotificationHandlers{svc: svc, access: access}
}

// canManage reports whether the caller may mutate notifications (admin, or holder
// of can_manage_notifications). It writes no response: a mutation turns a false
// into a 403, while a read path turns it into a narrower result set.
func (h *NotificationHandlers) canManage(c echo.Context, userID int64) (bool, error) {
	if h.access == nil {
		return false, nil // fail closed
	}
	return h.access.CanManageNotifications(c.Request().Context(), userID)
}

// requireManageNotifications is the in-handler half of the capability gate. On
// denial it writes the 403 and returns errAccessDenied, which callers MUST
// propagate (see monitor_access.go for why returning nil there was a real bug).
func (h *NotificationHandlers) requireManageNotifications(c echo.Context) (int64, error) {
	userID, ok := userIDFromContext(c)
	if !ok {
		_ = unauthenticated(c)
		return 0, errAccessDenied
	}
	allowed, err := h.canManage(c, userID)
	if err != nil || !allowed {
		_ = c.JSON(http.StatusForbidden, errorBody("insufficient permissions"))
		return 0, errAccessDenied
	}
	return userID, nil
}

// --- Request / response DTOs ---------------------------------------------

// CreateNotificationRequest is the body of POST /api/notifications.
type CreateNotificationRequest struct {
	Name          string         `json:"name"`
	Type          string         `json:"type"`
	Active        *bool          `json:"active"`
	IsDefault     bool           `json:"is_default"`
	IncludeAckURL *bool          `json:"include_ack_url"`
	TemplateID    *int64         `json:"template_id"`
	Config        map[string]any `json:"config"`
}

// UpdateNotificationRequest is the body of PUT /api/notifications/:id.
type UpdateNotificationRequest struct {
	Name          string         `json:"name"`
	Active        *bool          `json:"active"`
	IsDefault     *bool          `json:"is_default"`
	IncludeAckURL *bool          `json:"include_ack_url"`
	TemplateID    *int64         `json:"template_id"`
	Config        map[string]any `json:"config"`
}

// NotificationView is the wire shape of domain.Notification.
type NotificationView struct {
	ID            int64          `json:"id"`
	UserID        int64          `json:"user_id"`
	Name          string         `json:"name"`
	Type          string         `json:"type"`
	Active        bool           `json:"active"`
	IsDefault     bool           `json:"is_default"`
	IncludeAckURL bool           `json:"include_ack_url"`
	TemplateID    *int64         `json:"template_id"`
	Config        map[string]any `json:"config"`
	CreatedAt     string         `json:"created_at"`
	UpdatedAt     string         `json:"updated_at"`
}

func toNotificationView(n *domain.Notification) *NotificationView {
	if n == nil {
		return nil
	}
	return &NotificationView{
		ID:            n.ID,
		UserID:        n.UserID,
		Name:          n.Name,
		Type:          n.Type,
		Active:        n.Active,
		IsDefault:     n.IsDefault,
		IncludeAckURL: n.IncludeAckURL,
		TemplateID:    n.TemplateID,
		Config:        n.Config,
		CreatedAt:     n.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:     n.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// --- Notification CRUD ---------------------------------------------------

// Create handles POST /api/notifications. Requires can_manage_notifications
// (admins hold it implicitly).
func (h *NotificationHandlers) Create(c echo.Context) error {
	userID, err := h.requireManageNotifications(c)
	if err != nil {
		return err
	}

	var req CreateNotificationRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.Name == "" {
		return badRequest(c, "name is required")
	}
	if req.Type == "" {
		return badRequest(c, "type is required")
	}

	active := true
	if req.Active != nil {
		active = *req.Active
	}
	includeAckURL := domain.DefaultIncludeAckURL
	if req.IncludeAckURL != nil {
		includeAckURL = *req.IncludeAckURL
	}

	n := &domain.Notification{
		UserID:        userID,
		Name:          req.Name,
		Type:          req.Type,
		Active:        active,
		IsDefault:     req.IsDefault,
		IncludeAckURL: includeAckURL,
		TemplateID:    req.TemplateID,
		Config:        req.Config,
	}

	if err := h.svc.Create(c.Request().Context(), n); err != nil {
		return mapNotifError(c, err)
	}

	return c.JSON(http.StatusCreated, toNotificationView(n))
}

// List handles GET /api/notifications.
//
// Two different result sets, by capability:
//   - admin / can_manage_notifications → every notification in the install, because
//     that is exactly the set they are allowed to edit;
//   - everyone else → read-only, restricted to notifications attached to monitors
//     they can see. A user with no monitor grants gets [].
func (h *NotificationHandlers) List(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	if h.access == nil {
		return c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
	}

	manage, err := h.canManage(c, userID)
	if err != nil {
		return mapNotifError(c, err)
	}

	var notifications []*domain.Notification
	if manage {
		notifications, err = h.svc.ListAll(c.Request().Context())
	} else {
		all, visibleIDs, vErr := h.access.VisibleMonitorIDs(c.Request().Context(), userID)
		if vErr != nil {
			return mapNotifError(c, vErr)
		}
		if all {
			// all=true is admin or can_view_all_monitors. A view-all non-manager
			// does not hold can_manage, so this branch is reachable — widen to
			// the full list rather than silently showing nothing.
			notifications, err = h.svc.ListAll(c.Request().Context())
		} else {
			notifications, err = h.svc.ListForMonitors(c.Request().Context(), visibleIDs)
		}
	}
	if err != nil {
		return mapNotifError(c, err)
	}

	views := make([]*NotificationView, len(notifications))
	for i, n := range notifications {
		views[i] = toNotificationView(n)
	}
	return c.JSON(http.StatusOK, views)
}

// ListForMonitor handles GET /api/monitors/:monitorId/notifications.
// Returns the notifications assigned to a specific monitor (used by monitor detail page).
func (h *NotificationHandlers) ListForMonitor(c echo.Context) error {
	monitorID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor id")
	}
	if err := requireMonitorViewAccess(c, h.access, monitorID); err != nil {
		return err
	}

	// Delegate to service (GetByMonitorID already exists and is used in notification dispatch path).
	notifications, err := h.svc.GetByMonitorID(c.Request().Context(), monitorID)
	if err != nil {
		return mapNotifError(c, err)
	}

	views := make([]*NotificationView, len(notifications))
	for i, n := range notifications {
		views[i] = toNotificationView(n)
	}
	return c.JSON(http.StatusOK, views)
}

// canViewNotification reports whether the caller may READ one notification: they
// can manage notifications, or the notification is attached to a monitor they can
// see. Used to keep GET /:id consistent with the scoped List.
func (h *NotificationHandlers) canViewNotification(c echo.Context, userID, notifID int64) (bool, error) {
	manage, err := h.canManage(c, userID)
	if err != nil {
		return false, err
	}
	if manage {
		return true, nil
	}
	if h.access == nil {
		return false, nil
	}
	all, visibleIDs, err := h.access.VisibleMonitorIDs(c.Request().Context(), userID)
	if err != nil {
		return false, err
	}
	if all {
		return true, nil
	}
	links, err := h.svc.ListByNotification(c.Request().Context(), notifID)
	if err != nil {
		return false, err
	}
	visible := make(map[int64]bool, len(visibleIDs))
	for _, id := range visibleIDs {
		visible[id] = true
	}
	for _, l := range links {
		if visible[l.MonitorID] {
			return true, nil
		}
	}
	return false, nil
}

// GetByID handles GET /api/notifications/:id. 404 (not 403) when the caller may
// not see it, so we never confirm that a notification they cannot reach exists.
func (h *NotificationHandlers) GetByID(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid notification id")
	}

	allowed, err := h.canViewNotification(c, userID, id)
	if err != nil {
		return mapNotifError(c, err)
	}
	if !allowed {
		return c.JSON(http.StatusNotFound, errorBody(errNotificationNotFound))
	}

	n, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapNotifError(c, err)
	}

	return c.JSON(http.StatusOK, toNotificationView(n))
}

// Update handles PUT /api/notifications/:id. Requires can_manage_notifications;
// the notification's creator is irrelevant (see the type doc).
func (h *NotificationHandlers) Update(c echo.Context) error {
	if _, err := h.requireManageNotifications(c); err != nil {
		return err
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid notification id")
	}

	existing, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapNotifError(c, err)
	}

	var req UpdateNotificationRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.Active != nil {
		existing.Active = *req.Active
	}
	if req.IsDefault != nil {
		existing.IsDefault = *req.IsDefault
	}
	if req.IncludeAckURL != nil {
		existing.IncludeAckURL = *req.IncludeAckURL
	}
	// PUT replaces the selectable template association. A missing or explicit
	// null template_id means "use the provider default layout".
	existing.TemplateID = req.TemplateID
	if req.Config != nil {
		existing.Config = req.Config
	}

	if err := h.svc.Update(c.Request().Context(), existing); err != nil {
		return mapNotifError(c, err)
	}

	return c.JSON(http.StatusOK, toNotificationView(existing))
}

// Delete handles DELETE /api/notifications/:id. Requires can_manage_notifications.
func (h *NotificationHandlers) Delete(c echo.Context) error {
	if _, err := h.requireManageNotifications(c); err != nil {
		return err
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid notification id")
	}

	if _, err := h.svc.GetByID(c.Request().Context(), id); err != nil {
		return mapNotifError(c, err)
	}

	if err := h.svc.Delete(c.Request().Context(), id); err != nil {
		return mapNotifError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

// --- Monitor-Notification Association ------------------------------------

// AttachToMonitor handles POST /api/notifications/:id/monitor/:monitorId.
//
// Two gates, both required: the caller must hold can_manage_notifications, AND
// must be able to VIEW the monitor. Without the second gate a capability holder
// could wire a notification onto a monitor they were never granted and start
// receiving alerts about it.
func (h *NotificationHandlers) AttachToMonitor(c echo.Context) error {
	if _, err := h.requireManageNotifications(c); err != nil {
		return err
	}

	notifID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid notification id")
	}
	monitorID, err := strconv.ParseInt(c.Param("monitorId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor id")
	}

	if _, err := h.svc.GetByID(c.Request().Context(), notifID); err != nil {
		return mapNotifError(c, err)
	}
	if err := requireMonitorViewAccess(c, h.access, monitorID); err != nil {
		return err
	}

	if err := h.svc.AttachToMonitor(c.Request().Context(), monitorID, notifID); err != nil {
		return mapNotifError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

// DetachFromMonitor handles DELETE /api/notifications/:id/monitor/:monitorId.
// Same two gates as AttachToMonitor.
func (h *NotificationHandlers) DetachFromMonitor(c echo.Context) error {
	if _, err := h.requireManageNotifications(c); err != nil {
		return err
	}

	notifID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid notification id")
	}
	monitorID, err := strconv.ParseInt(c.Param("monitorId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor id")
	}

	if _, err := h.svc.GetByID(c.Request().Context(), notifID); err != nil {
		return mapNotifError(c, err)
	}
	if err := requireMonitorViewAccess(c, h.access, monitorID); err != nil {
		return err
	}

	if err := h.svc.DetachFromMonitor(c.Request().Context(), monitorID, notifID); err != nil {
		return mapNotifError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

// --- Group (folder) assignment -------------------------------------------
//
// Attaching a notification to a GROUP means "alert me when this folder as a whole
// trips", per the folder's own condition (worst_of_children / threshold / …). It
// does NOT inherit down to the monitors inside it — they alert through their own
// attachments, independently. See services.GroupAlertService.

// AttachToGroup handles POST /api/notifications/:id/group/:groupId. Same two gates
// as AttachToMonitor: the can_manage_notifications capability, plus view access to
// the folder being pointed at (404, never 403).
func (h *NotificationHandlers) AttachToGroup(c echo.Context) error {
	if _, err := h.requireManageNotifications(c); err != nil {
		return err
	}

	notifID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid notification id")
	}
	groupID, err := strconv.ParseInt(c.Param("groupId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid group id")
	}

	if _, err := h.svc.GetByID(c.Request().Context(), notifID); err != nil {
		return mapNotifError(c, err)
	}
	if err := requireGroupViewAccess(c, h.access, groupID); err != nil {
		return err
	}

	if err := h.svc.AttachToGroup(c.Request().Context(), groupID, notifID); err != nil {
		return mapNotifError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

// DetachFromGroup handles DELETE /api/notifications/:id/group/:groupId.
// Same two gates as AttachToGroup.
func (h *NotificationHandlers) DetachFromGroup(c echo.Context) error {
	if _, err := h.requireManageNotifications(c); err != nil {
		return err
	}

	notifID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid notification id")
	}
	groupID, err := strconv.ParseInt(c.Param("groupId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid group id")
	}

	if _, err := h.svc.GetByID(c.Request().Context(), notifID); err != nil {
		return mapNotifError(c, err)
	}
	if err := requireGroupViewAccess(c, h.access, groupID); err != nil {
		return err
	}

	if err := h.svc.DetachFromGroup(c.Request().Context(), groupID, notifID); err != nil {
		return mapNotifError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

// ListForGroup handles GET /api/monitor-groups/:id/notifications — the folder's
// attached providers, for the group form's checkbox list.
//
// Read gate is view access to the folder, not the manage capability: a user who
// can see the folder may see what it alerts through, exactly as ListForMonitor
// works for a monitor.
func (h *NotificationHandlers) ListForGroup(c echo.Context) error {
	groupID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid group id")
	}
	if err := requireGroupViewAccess(c, h.access, groupID); err != nil {
		return err
	}

	notifications, err := h.svc.GetByGroupID(c.Request().Context(), groupID)
	if err != nil {
		return mapNotifError(c, err)
	}

	views := make([]*NotificationView, len(notifications))
	for i, n := range notifications {
		views[i] = toNotificationView(n)
	}
	return c.JSON(http.StatusOK, views)
}

// --- Test Notification ---------------------------------------------------

// Test handles POST /api/notifications/:id/test. Requires
// can_manage_notifications: firing a test message is a side effect on the outside
// world (an email, a Slack post), not a read.
func (h *NotificationHandlers) Test(c echo.Context) error {
	if _, err := h.requireManageNotifications(c); err != nil {
		return err
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid notification id")
	}

	n, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapNotifError(c, err)
	}

	if err := h.svc.SendTest(c.Request().Context(), n); err != nil {
		return c.JSON(http.StatusInternalServerError, errorBody("test send failed: "+err.Error()))
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "sent"})
}

// --- Error translation helper -------------------------------------------

func mapNotifError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound) || errors.Is(err, ports.ErrNotFound):
		return c.JSON(http.StatusNotFound, errorBody(errNotificationNotFound))
	case errors.Is(err, domain.ErrValidation):
		return c.JSON(http.StatusBadRequest, errorBody(err.Error()))
	default:
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
}
