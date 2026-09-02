package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// errMaintenanceNotFound is the client-facing message used whenever a
// maintenance window doesn't exist or isn't owned by the caller. Reusing a
// single message avoids leaking whether the ID exists under another user.
const errMaintenanceNotFound = "maintenance window not found"

// MaintenanceHandlers groups maintenance window CRUD and monitor assignment endpoints.
//
// RBAC. Maintenance windows are install-wide objects, exactly like notifications:
//
//   - Admins and users holding can_manage_maintenance may create, update, delete
//     and re-target ANY window, whoever created it.
//   - Everyone else gets a read-only view restricted to the windows covering
//     monitors they can see.
//
// The access service is also what stops a capability holder from pointing a window
// at a monitor they were never granted: a window SUPPRESSES ALERTS for the monitors
// linked to it, so attaching someone else's monitor would silence its alerts. That
// check used to be "do you own this monitor?"; it is now "can you view it?".
type MaintenanceHandlers struct {
	svc    *services.MaintenanceService
	access *services.AccessService
}

// NewMaintenanceHandlers creates handlers bound to the supplied services.
func NewMaintenanceHandlers(svc *services.MaintenanceService, access *services.AccessService) *MaintenanceHandlers {
	return &MaintenanceHandlers{svc: svc, access: access}
}

// canManage reports whether the caller may mutate maintenance windows (admin, or
// holder of can_manage_maintenance). Writes no response.
func (h *MaintenanceHandlers) canManage(c echo.Context, userID int64) (bool, error) {
	if h.access == nil {
		return false, nil // fail closed
	}
	return h.access.CanManageMaintenance(c.Request().Context(), userID)
}

// requireManageMaintenance is the in-handler half of the capability gate. On
// denial it writes the 403 and returns errAccessDenied, which callers MUST
// propagate.
func (h *MaintenanceHandlers) requireManageMaintenance(c echo.Context) (int64, error) {
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

// visibleMonitorIDs returns the requested monitor IDs, rejecting the request if the
// caller cannot VIEW any of them. Duplicate IDs are collapsed. A nil/empty list is
// valid and means "this window covers no monitors" (and so suppresses nothing).
//
// On rejection it has already written the 404 response; the returned error must be
// propagated verbatim by the caller.
func (h *MaintenanceHandlers) visibleMonitorIDs(c echo.Context, userID int64, requested []int64) ([]int64, error) {
	if len(requested) == 0 {
		return nil, nil
	}
	if h.access == nil {
		return nil, c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
	}
	all, visibleIDs, err := h.access.VisibleMonitorIDs(c.Request().Context(), userID)
	if err != nil {
		return nil, mapMaintenanceError(c, err)
	}
	visible := make(map[int64]struct{}, len(visibleIDs))
	for _, id := range visibleIDs {
		visible[id] = struct{}{}
	}

	seen := make(map[int64]struct{}, len(requested))
	out := make([]int64, 0, len(requested))
	for _, id := range requested {
		if _, ok := visible[id]; !all && !ok {
			// Same message whether the monitor is missing or simply not visible to
			// this caller — never reveal that a monitor they cannot see exists.
			return nil, c.JSON(http.StatusNotFound, errorBody("monitor not found"))
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// MaintenanceView is the wire shape of domain.MaintenanceWindow.
//
// The domain type carries no json tags, so returning it directly emitted
// capitalized Go field names (Title, StartDate, …) while the rest of the API —
// and the frontend's MaintenanceWindow type — use snake_case. See the
// Wire-Shape Discipline rule in AGENTS.md: never serialize a domain type.
//
// StartDate/EndDate are pointers so a cron-strategy window (which has no dates)
// serializes them as null instead of the zero time "0001-01-01T00:00:00Z".
// MonitorIDs is the set of monitors the window suppresses alerts for. It is
// always a non-nil slice so the field serializes as [] rather than null — the
// frontend distinguishes "covers no monitors" from "not loaded" by length.
type MaintenanceView struct {
	ID          int64   `json:"id"`
	UserID      int64   `json:"user_id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Active      bool    `json:"active"`
	Strategy    string  `json:"strategy"`
	StartDate   *string `json:"start_date"`
	EndDate     *string `json:"end_date"`
	CronExpr    string  `json:"cron_expr"`
	Duration    int     `json:"duration"`
	// Timezone is an IANA name for cron evaluation. Empty/legacy → "UTC".
	Timezone   string  `json:"timezone"`
	MonitorIDs []int64 `json:"monitor_ids"`
}

func toMaintenanceView(mw *domain.MaintenanceWindow, monitorIDs []int64) *MaintenanceView {
	if mw == nil {
		return nil
	}
	if monitorIDs == nil {
		monitorIDs = []int64{}
	}
	tz := mw.Timezone
	if tz == "" {
		tz = "UTC"
	}
	return &MaintenanceView{
		ID:          mw.ID,
		UserID:      mw.UserID,
		Title:       mw.Title,
		Description: mw.Description,
		Active:      mw.Active,
		Strategy:    mw.Strategy,
		StartDate:   formatOptionalTime(&mw.StartDate),
		EndDate:     formatOptionalTime(&mw.EndDate),
		CronExpr:    mw.CronExpr,
		Duration:    mw.Duration,
		Timezone:    tz,
		MonitorIDs:  monitorIDs,
	}
}

// toMaintenanceViews resolves each window's monitor links. The per-window lookup
// is an N+1, but a user's window count is small and bounded; if that stops being
// true, add a batch ListByMaintenanceIDs to the link repo.
func (h *MaintenanceHandlers) toMaintenanceViews(ctx context.Context, windows []*domain.MaintenanceWindow) []*MaintenanceView {
	out := make([]*MaintenanceView, 0, len(windows))
	for _, w := range windows {
		ids, err := h.svc.ListMonitorIDs(ctx, w.ID)
		if err != nil {
			// A link-lookup failure shouldn't blank the whole list; degrade to
			// "no monitors" for this row and record why.
			slog.Warn("maintenance: list monitor links failed", "maintenance_id", w.ID, "error", err)
			ids = nil
		}
		out = append(out, toMaintenanceView(w, ids))
	}
	return out
}

// Create handles POST /api/maintenance. Requires can_manage_maintenance (admins
// hold it implicitly).
func (h *MaintenanceHandlers) Create(c echo.Context) error {
	userID, err := h.requireManageMaintenance(c)
	if err != nil {
		return err
	}

	var req struct {
		Title       string    `json:"title"`
		Description string    `json:"description"`
		Strategy    string    `json:"strategy"`
		StartDate   time.Time `json:"start_date"`
		EndDate     time.Time `json:"end_date"`
		CronExpr    string    `json:"cron_expr"`
		Duration    int       `json:"duration"`
		Timezone    string    `json:"timezone"`
		MonitorIDs  []int64   `json:"monitor_ids"`
	}
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request")
	}

	// Validate the monitor set BEFORE creating the window, so a request naming a
	// monitor the caller cannot see doesn't leave an orphan window behind.
	monitorIDs, err := h.visibleMonitorIDs(c, userID, req.MonitorIDs)
	if err != nil {
		return err
	}

	mw := &domain.MaintenanceWindow{
		UserID:      userID,
		Title:       req.Title,
		Description: req.Description,
		Strategy:    req.Strategy,
		StartDate:   req.StartDate,
		EndDate:     req.EndDate,
		CronExpr:    req.CronExpr,
		Duration:    req.Duration,
		Timezone:    req.Timezone,
		Active:      true,
	}
	if err := h.svc.Create(c.Request().Context(), mw); err != nil {
		return mapMaintenanceError(c, err)
	}
	if err := h.svc.SetMonitors(c.Request().Context(), mw.ID, monitorIDs); err != nil {
		return mapMaintenanceError(c, err)
	}
	// Best-effort announcement after links are committed (Track B subscription mail).
	h.svc.NotifyScheduled(c.Request().Context(), mw, monitorIDs)
	return c.JSON(http.StatusCreated, toMaintenanceView(mw, monitorIDs))
}

// List handles GET /api/maintenance
//
// Two different result sets, by capability:
//   - admin / can_manage_maintenance → every window in the install, because that is
//     exactly the set they are allowed to edit;
//   - everyone else → read-only, restricted to the windows covering monitors they
//     can see. A user with no monitor grants gets [].
func (h *MaintenanceHandlers) List(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	if h.access == nil {
		return c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
	}

	manage, err := h.canManage(c, userID)
	if err != nil {
		return mapMaintenanceError(c, err)
	}

	var windows []*domain.MaintenanceWindow
	if manage {
		windows, err = h.svc.ListAll(c.Request().Context())
	} else {
		all, visibleIDs, vErr := h.access.VisibleMonitorIDs(c.Request().Context(), userID)
		if vErr != nil {
			return mapMaintenanceError(c, vErr)
		}
		if all {
			// all=true is admin or can_view_all_monitors. A view-all non-manager
			// hits this branch; widen rather than silently showing nothing.
			windows, err = h.svc.ListAll(c.Request().Context())
		} else {
			windows, err = h.svc.ListForMonitors(c.Request().Context(), visibleIDs)
		}
	}
	if err != nil {
		return mapMaintenanceError(c, err)
	}
	return c.JSON(http.StatusOK, h.toMaintenanceViews(c.Request().Context(), windows))
}

// canViewMaintenance reports whether the caller may READ one window: they can
// manage maintenance, or the window covers a monitor they can see.
func (h *MaintenanceHandlers) canViewMaintenance(c echo.Context, userID, maintenanceID int64) (bool, error) {
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
	linked, err := h.svc.ListMonitorIDs(c.Request().Context(), maintenanceID)
	if err != nil {
		return false, err
	}
	visible := make(map[int64]bool, len(visibleIDs))
	for _, id := range visibleIDs {
		visible[id] = true
	}
	for _, id := range linked {
		if visible[id] {
			return true, nil
		}
	}
	return false, nil
}

// Get handles GET /api/maintenance/:id. 404 (not 403) when the caller may not see
// the window, so we never confirm that one they cannot reach exists.
func (h *MaintenanceHandlers) Get(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid id")
	}

	mw, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapMaintenanceError(c, err)
	}
	allowed, err := h.canViewMaintenance(c, userID, id)
	if err != nil {
		return mapMaintenanceError(c, err)
	}
	if !allowed {
		return c.JSON(http.StatusNotFound, errorBody(errMaintenanceNotFound))
	}
	monitorIDs, err := h.svc.ListMonitorIDs(c.Request().Context(), id)
	if err != nil {
		return mapMaintenanceError(c, err)
	}
	return c.JSON(http.StatusOK, toMaintenanceView(mw, monitorIDs))
}

// Update handles PUT /api/maintenance/:id. Requires can_manage_maintenance; the
// window's creator is irrelevant (see the type doc).
func (h *MaintenanceHandlers) Update(c echo.Context) error {
	userID, err := h.requireManageMaintenance(c)
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid id")
	}

	existing, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapMaintenanceError(c, err)
	}

	var req struct {
		Title       string    `json:"title"`
		Description string    `json:"description"`
		Strategy    string    `json:"strategy"`
		StartDate   time.Time `json:"start_date"`
		EndDate     time.Time `json:"end_date"`
		CronExpr    string    `json:"cron_expr"`
		Duration    int       `json:"duration"`
		Timezone    string    `json:"timezone"`
		Active      bool      `json:"active"`
		MonitorIDs  []int64   `json:"monitor_ids"`
	}
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request")
	}

	monitorIDs, err := h.visibleMonitorIDs(c, userID, req.MonitorIDs)
	if err != nil {
		return err
	}

	mw := &domain.MaintenanceWindow{
		ID:          id,
		UserID:      existing.UserID,
		Title:       req.Title,
		Description: req.Description,
		Strategy:    req.Strategy,
		StartDate:   req.StartDate,
		EndDate:     req.EndDate,
		CronExpr:    req.CronExpr,
		Duration:    req.Duration,
		Timezone:    req.Timezone,
		Active:      req.Active,
	}
	if err := h.svc.Update(c.Request().Context(), mw); err != nil {
		return mapMaintenanceError(c, err)
	}
	// The form always submits the full monitor set, so this is a replace.
	if err := h.svc.SetMonitors(c.Request().Context(), id, monitorIDs); err != nil {
		return mapMaintenanceError(c, err)
	}
	// Reschedule announcement (best-effort) after links are committed.
	h.svc.NotifyScheduled(c.Request().Context(), mw, monitorIDs)
	return c.JSON(http.StatusOK, toMaintenanceView(mw, monitorIDs))
}

// Delete handles DELETE /api/maintenance/:id. Requires can_manage_maintenance.
func (h *MaintenanceHandlers) Delete(c echo.Context) error {
	if _, err := h.requireManageMaintenance(c); err != nil {
		return err
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid id")
	}

	// Confirm it exists so deleting an unknown id is a 404, not a 204.
	if _, err := h.svc.GetByID(c.Request().Context(), id); err != nil {
		return mapMaintenanceError(c, err)
	}

	if err := h.svc.Delete(c.Request().Context(), id); err != nil {
		return mapMaintenanceError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// AssignMonitor handles POST /api/maintenance/:id/monitors. Requires
// can_manage_maintenance AND view access to the monitor being attached.
func (h *MaintenanceHandlers) AssignMonitor(c echo.Context) error {
	userID, err := h.requireManageMaintenance(c)
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid id")
	}
	if _, err := h.svc.GetByID(c.Request().Context(), id); err != nil {
		return mapMaintenanceError(c, err)
	}

	var req struct {
		MonitorID int64 `json:"monitor_id"`
	}
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request")
	}
	// Attaching a monitor you cannot see would let you suppress its alerts.
	ids, err := h.visibleMonitorIDs(c, userID, []int64{req.MonitorID})
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return badRequest(c, "monitor_id is required")
	}
	if err := h.svc.AssignMonitor(c.Request().Context(), id, ids[0]); err != nil {
		return mapMaintenanceError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// UnassignMonitor handles DELETE /api/maintenance/:id/monitors/:monitor_id.
// Requires can_manage_maintenance.
func (h *MaintenanceHandlers) UnassignMonitor(c echo.Context) error {
	if _, err := h.requireManageMaintenance(c); err != nil {
		return err
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid id")
	}
	monitorID, err := strconv.ParseInt(c.Param("monitor_id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor id")
	}
	if _, err := h.svc.GetByID(c.Request().Context(), id); err != nil {
		return mapMaintenanceError(c, err)
	}
	// No view check on the monitor here, deliberately: detaching only ever NARROWS
	// suppression, so it cannot be used to silence anything the caller shouldn't be
	// able to silence.
	if err := h.svc.UnassignMonitor(c.Request().Context(), id, monitorID); err != nil {
		return mapMaintenanceError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// ListMonitors handles GET /api/maintenance/:id/monitors
func (h *MaintenanceHandlers) ListMonitors(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid id")
	}
	if _, err := h.svc.GetByID(c.Request().Context(), id); err != nil {
		return mapMaintenanceError(c, err)
	}
	allowed, err := h.canViewMaintenance(c, userID, id)
	if err != nil {
		return mapMaintenanceError(c, err)
	}
	if !allowed {
		return c.JSON(http.StatusNotFound, errorBody(errMaintenanceNotFound))
	}
	monitorIDs, err := h.svc.ListMonitorIDs(c.Request().Context(), id)
	if err != nil {
		return mapMaintenanceError(c, err)
	}
	if monitorIDs == nil {
		monitorIDs = []int64{}
	}
	return c.JSON(http.StatusOK, monitorIDs)
}

// --- Error translation helper ---------------------------------------------

// mapMaintenanceError translates a service/repository error into the app's
// {"error": "..."} JSON envelope instead of leaking a bare Go error (which
// Echo's default handler renders as {"message":"Internal Server Error"}).
// Mirrors mapMonitorError in monitor.go.
func mapMaintenanceError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound) || errors.Is(err, ports.ErrNotFound):
		return c.JSON(http.StatusNotFound, errorBody(errMaintenanceNotFound))
	case errors.Is(err, domain.ErrValidation):
		return c.JSON(http.StatusBadRequest, errorBody(err.Error()))
	default:
		slog.Error("maintenance handler error", "error", err)
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
}
