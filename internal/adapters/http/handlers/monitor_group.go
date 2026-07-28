package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// errMonitorGroupNotFound is the client-facing message used whenever a
// monitor group doesn't exist or isn't owned by the caller. Reusing a single
// message avoids leaking whether the ID exists under another user (same
// pattern as errMaintenanceNotFound / errProxyNotFound).
const errMonitorGroupNotFound = "monitor group not found"

// MonitorGroupHandlers groups the monitor-group (folder) CRUD endpoints
// behind a single receiver.
//
// RBAC: reads are scoped by services.AccessService (an admin sees every group in
// the install; a non-admin sees the granted ones plus whatever is needed to make
// that tree coherent). Creating is gated in the router by the create_groups
// capability; changing an EXISTING group is gated here by requireGroupEditAccess,
// which admits admins and the group's creator only.
type MonitorGroupHandlers struct {
	svc    *services.MonitorGroupService
	access *services.AccessService
}

// NewMonitorGroupHandlers creates a MonitorGroupHandlers bound to the
// supplied services.
func NewMonitorGroupHandlers(svc *services.MonitorGroupService, access *services.AccessService) *MonitorGroupHandlers {
	return &MonitorGroupHandlers{svc: svc, access: access}
}

// --- Request / response DTOs ---------------------------------------------

// upsertMonitorGroupRequest is the shared body shape for POST/PUT
// /api/monitor-groups.
type upsertMonitorGroupRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// ParentID nests this group inside another group. On update it is always
	// applied (not gated on non-zero/nil) so the client can move a group back
	// to top level by sending parent_id: null — same semantics
	// MonitorHandlers uses for group_id.
	ParentID           *int64                `json:"parent_id"`
	Condition          domain.GroupCondition `json:"condition"`
	Threshold          int                   `json:"threshold"`
	ThresholdIsPercent bool                  `json:"threshold_is_percent"`
	Weight             int                   `json:"weight"`
	Collapsed          bool                  `json:"collapsed"`
}

// MonitorGroupView is the wire shape of domain.MonitorGroup.
type MonitorGroupView struct {
	ID                 int64                 `json:"id"`
	Name               string                `json:"name"`
	Description        string                `json:"description"`
	ParentID           *int64                `json:"parent_id"`
	Condition          domain.GroupCondition `json:"condition"`
	Threshold          int                   `json:"threshold"`
	ThresholdIsPercent bool                  `json:"threshold_is_percent"`
	Weight             int                   `json:"weight"`
	Collapsed          bool                  `json:"collapsed"`
	// Status is the derived status (domain.Status: 0=DOWN 1=UP 2=PENDING
	// 3=MAINTENANCE) for this group. It is a pointer WITHOUT omitempty:
	// status 0 (DOWN) is a legitimate value that omitempty would silently
	// swallow, turning a down folder into one with no status at all. nil
	// means the group has no derived status (an "ignore" group, or a group
	// with no children) and the frontend should render it as a plain folder.
	Status    *int   `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// toMonitorGroupView projects a domain.MonitorGroup to the public DTO.
// status is nil unless the caller has resolved one (currently only List does).
func toMonitorGroupView(g *domain.MonitorGroup, status *int) *MonitorGroupView {
	if g == nil {
		return nil
	}
	return &MonitorGroupView{
		ID:                 g.ID,
		Name:               g.Name,
		Description:        g.Description,
		ParentID:           g.ParentID,
		Condition:          g.Condition,
		Threshold:          g.Threshold,
		ThresholdIsPercent: g.ThresholdIsPercent,
		Weight:             g.Weight,
		Collapsed:          g.Collapsed,
		Status:             status,
		CreatedAt:          g.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:          g.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// --- Handlers -----------------------------------------------------------

// grantCreatorAccess gives the creator a view grant on the group they just made,
// so it appears in their tree and in the admin permission editor.
//
// The grant is DEEP. A creator's own folder is the one case where recursion is
// not a judgement call: they are about to file monitors and subgroups under it,
// and a shallow grant would hide every subgroup they then create inside their
// own folder. An admin can narrow it afterwards in the permission editor; the
// reverse default would make the folder feel broken the moment it was used.
//
// Failure is logged, never returned — see MonitorHandlers.grantCreatorAccess for
// why a post-create grant must not turn a successful create into a 500.
func (h *MonitorGroupHandlers) grantCreatorAccess(c echo.Context, userID, groupID int64) {
	if h.access == nil {
		return
	}
	if err := h.access.GrantGroup(c.Request().Context(), userID, groupID, true); err != nil {
		slog.ErrorContext(c.Request().Context(), "auto-grant creator view access to new monitor group failed",
			"user_id", userID, "group_id", groupID, "error", err)
	}
}

// Create handles POST /api/monitor-groups. Requires the create_groups capability
// (router-gated); admins always hold it.
//
// The creator becomes the group's owner via MonitorGroup.UserID — that is what
// later lets them edit and delete it — and is auto-granted a deep view of it.
func (h *MonitorGroupHandlers) Create(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}

	var req upsertMonitorGroupRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	g := &domain.MonitorGroup{
		UserID:             userID,
		Name:               req.Name,
		Description:        req.Description,
		ParentID:           req.ParentID,
		Condition:          req.Condition,
		Threshold:          req.Threshold,
		ThresholdIsPercent: req.ThresholdIsPercent,
		Weight:             req.Weight,
		Collapsed:          req.Collapsed,
	}

	if err := h.svc.Create(c.Request().Context(), g); err != nil {
		return mapMonitorGroupError(c, err)
	}
	h.grantCreatorAccess(c, userID, g.ID)

	return c.JSON(http.StatusCreated, toMonitorGroupView(g, nil))
}

// List handles GET /api/monitor-groups.
//
// RBAC: an admin gets every group in the install; a non-admin gets exactly the
// groups AccessService says are visible to them (granted groups + their
// descendants, the folders holding monitors they can see, and the ancestors of
// both, so the tree always renders without a dangling parent).
//
// Statuses are rolled up over the WHOLE tree, not just the visible slice: a
// folder's status has to account for every child, or a folder holding one visible
// UP monitor and one invisible DOWN monitor would report UP. Visibility decides
// which rows are returned, never how they are computed.
func (h *MonitorGroupHandlers) List(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	if h.access == nil {
		return c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
	}

	all, visibleIDs, err := h.access.VisibleGroupIDs(c.Request().Context(), userID)
	if err != nil {
		return mapMonitorGroupError(c, err)
	}

	groups, err := h.svc.ListAll(c.Request().Context())
	if err != nil {
		return mapMonitorGroupError(c, err)
	}
	if !all {
		visible := make(map[int64]bool, len(visibleIDs))
		for _, id := range visibleIDs {
			visible[id] = true
		}
		filtered := make([]*domain.MonitorGroup, 0, len(visibleIDs))
		for _, g := range groups {
			if visible[g.ID] {
				filtered = append(filtered, g)
			}
		}
		groups = filtered
	}

	// userID 0 == "the whole install" — see MonitorGroupService.ResolveStatuses.
	statuses, err := h.svc.ResolveStatuses(c.Request().Context(), 0)
	if err != nil {
		return mapMonitorGroupError(c, err)
	}

	views := make([]*MonitorGroupView, len(groups))
	for i, g := range groups {
		var status *int
		if s, ok := statuses[g.ID]; ok {
			v := int(s)
			status = &v
		}
		views[i] = toMonitorGroupView(g, status)
	}
	return c.JSON(http.StatusOK, views)
}

// GetByID handles GET /api/monitor-groups/:id.
func (h *MonitorGroupHandlers) GetByID(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	if h.access == nil {
		return c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor group id")
	}

	// 404-not-403, same as monitors: never confirm a group the caller cannot see.
	allowed, err := h.access.CanViewGroup(c.Request().Context(), userID, id)
	if err != nil {
		return mapMonitorGroupError(c, err)
	}
	if !allowed {
		return c.JSON(http.StatusNotFound, errorBody(errMonitorGroupNotFound))
	}

	g, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapMonitorGroupError(c, err)
	}

	return c.JSON(http.StatusOK, toMonitorGroupView(g, nil))
}

// Update handles PUT /api/monitor-groups/:id. Admins, or the user who created
// this group. Gated in the handler, not the router: the answer depends on who
// owns THIS group. See MonitorHandlers.Update.
func (h *MonitorGroupHandlers) Update(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor group id")
	}
	if err := requireGroupEditAccess(c, h.access, id); err != nil {
		return err
	}

	existing, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapMonitorGroupError(c, err)
	}

	var req upsertMonitorGroupRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	existing.Name = req.Name
	existing.Description = req.Description
	// ParentID is always applied (not gated on non-zero/nil) so the client
	// can explicitly move this group back to top level by sending
	// parent_id: null.
	existing.ParentID = req.ParentID
	existing.Condition = req.Condition
	existing.Threshold = req.Threshold
	existing.ThresholdIsPercent = req.ThresholdIsPercent
	existing.Weight = req.Weight
	existing.Collapsed = req.Collapsed

	if err := h.svc.Update(c.Request().Context(), existing); err != nil {
		return mapMonitorGroupError(c, err)
	}

	return c.JSON(http.StatusOK, toMonitorGroupView(existing, nil))
}

// Delete handles DELETE /api/monitor-groups/:id. Admin-only. It removes the group
// only — monitors and subgroups filed under it are re-homed to its parent, never
// deleted (see ports.MonitorGroupRepository.Delete).
func (h *MonitorGroupHandlers) Delete(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor group id")
	}
	if err := requireGroupEditAccess(c, h.access, id); err != nil {
		return err
	}

	// Confirm it exists so deleting an unknown id is a 404, not a 204.
	if _, err := h.svc.GetByID(c.Request().Context(), id); err != nil {
		return mapMonitorGroupError(c, err)
	}

	if err := h.svc.Delete(c.Request().Context(), id); err != nil {
		return mapMonitorGroupError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

// --- Error translation helper -------------------------------------------

// mapMonitorGroupError translates a service/repository error into the app's
// {"error": "..."} JSON envelope. Mirrors mapMonitorError/mapMaintenanceError.
func mapMonitorGroupError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound) || errors.Is(err, ports.ErrNotFound):
		return c.JSON(http.StatusNotFound, errorBody(errMonitorGroupNotFound))
	case errors.Is(err, domain.ErrValidation):
		return c.JSON(http.StatusBadRequest, errorBody(err.Error()))
	default:
		slog.Error("monitor group handler error", "error", err)
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
}
