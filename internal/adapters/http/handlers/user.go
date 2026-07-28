package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// UserHandlers groups the admin user-management endpoints (POST/GET/PUT/
// DELETE /api/users) plus the RBAC permission endpoints
// (GET/PUT /api/users/:id/permissions).
//
// Every route here is admin-only, enforced by middleware.RequireAdmin in the
// router after middleware.SessionOrAPIKey has resolved the principal (a session
// JWT, or an API key with the "write" scope).
type UserHandlers struct {
	svc    *services.AuthService
	access *services.AccessService
}

// NewUserHandlers creates user-management handlers bound to authSvc and the
// access service (which owns the grant read/write path).
func NewUserHandlers(svc *services.AuthService, access *services.AccessService) *UserHandlers {
	return &UserHandlers{svc: svc, access: access}
}

// CreateUserRequest is the body of POST /api/users.
//
// The four Can* fields are the capability flags a non-admin can hold; omitted
// means false. They are meaningless for an admin, who implicitly holds all of
// them.
type CreateUserRequest struct {
	Username               string `json:"username"`
	Password               string `json:"password"`
	Active                 *bool  `json:"active"`
	IsAdmin                *bool  `json:"is_admin"`
	CanManageNotifications *bool  `json:"can_manage_notifications"`
	CanManageMaintenance   *bool  `json:"can_manage_maintenance"`
	CanCreateMonitors      *bool  `json:"can_create_monitors"`
	CanCreateGroups        *bool  `json:"can_create_groups"`
	Timezone               string `json:"timezone"`
}

// UpdateUserRequest is the body of PUT /api/users/:id. A nil field leaves
// the corresponding value unchanged; a non-nil Password resets it.
type UpdateUserRequest struct {
	Username               *string `json:"username"`
	Active                 *bool   `json:"active"`
	IsAdmin                *bool   `json:"is_admin"`
	CanManageNotifications *bool   `json:"can_manage_notifications"`
	CanManageMaintenance   *bool   `json:"can_manage_maintenance"`
	CanCreateMonitors      *bool   `json:"can_create_monitors"`
	CanCreateGroups        *bool   `json:"can_create_groups"`
	Timezone               *string `json:"timezone"`
	Password               *string `json:"password"`
}

// GroupGrantView is one group grant on the wire: which folder, and how far down
// it reaches.
type GroupGrantView struct {
	GroupID int64 `json:"group_id"`
	// IncludeDescendants deep-grants the subtree. On the way OUT it is always
	// populated. On the way IN it is a *bool so that omitting it can mean "deep"
	// rather than Go's zero value of false — see updatePermissionsRequest.groups.
	IncludeDescendants bool `json:"include_descendants"`
}

// UserPermissionsView is the body of GET /api/users/:id/permissions and the
// shape PUT returns.
//
// These are the DIRECT grants only — the raw rows an admin edits. The expanded
// set (a group grant pulling in its subgroups and their monitors) is derived at
// request time by services.AccessService and is deliberately not echoed here:
// showing the expansion in the editor would invite an admin to "fix" it by hand
// and drift the two apart.
//
// Both slices are always non-nil, so they serialize as [] and never null.
type UserPermissionsView struct {
	MonitorIDs []int64          `json:"monitor_ids"`
	Groups     []GroupGrantView `json:"groups"`
}

// updatePermissionsRequest is what PUT /api/users/:id/permissions ACCEPTS, which
// is deliberately not the same shape it returns.
//
// Groups is the current field. GroupIDs is the pre-011 spelling, kept working
// for API clients outside this repo (nothing in-tree sends it any more — the
// admin UI was migrated with the field). A bare list of ids has an unambiguous
// reading: pre-011 group grants were always recursive, so a legacy id means a
// DEEP grant. Anything else would silently narrow such a caller's grants the
// moment they upgraded.
//
// Both are pointers so "absent" is distinguishable from "empty". Empty is
// meaningful on a replace-set endpoint — it is how you revoke the last grant —
// so treating [] as absent would make that impossible.
type updatePermissionsRequest struct {
	MonitorIDs []int64 `json:"monitor_ids"`
	Groups     *[]struct {
		GroupID int64 `json:"group_id"`
		// A *bool, and nil means DEEP. Go's zero value for bool is false, which
		// here would mean shallow — so a client that omits the field would
		// silently get the narrow grant, and the omission would look like a
		// choice. Deep matches the column default in migration 011 and the
		// always-recursive behavior that predates it.
		IncludeDescendants *bool `json:"include_descendants"`
	} `json:"groups"`
	GroupIDs *[]int64 `json:"group_ids"` // deprecated: use groups
}

// groupGrants resolves the request's group grants into the service's shape,
// reconciling the current and legacy fields.
//
// Sending both is rejected rather than merged or silently preferring one. On a
// replace-set endpoint the two cannot be combined coherently — each claims to be
// the complete set — and picking a winner would quietly discard half of what an
// admin asked for.
func (r updatePermissionsRequest) groupGrants() ([]services.GroupGrant, error) {
	if r.Groups != nil && r.GroupIDs != nil {
		return nil, fmt.Errorf("send either groups or group_ids, not both")
	}

	if r.Groups != nil {
		out := make([]services.GroupGrant, 0, len(*r.Groups))
		for _, g := range *r.Groups {
			deep := true // omitted => deep; see the field comment
			if g.IncludeDescendants != nil {
				deep = *g.IncludeDescendants
			}
			out = append(out, services.GroupGrant{GroupID: g.GroupID, IncludeDescendants: deep})
		}
		return out, nil
	}

	if r.GroupIDs != nil {
		out := make([]services.GroupGrant, 0, len(*r.GroupIDs))
		for _, id := range *r.GroupIDs {
			out = append(out, services.GroupGrant{GroupID: id, IncludeDescendants: true})
		}
		return out, nil
	}

	// Neither field sent. This endpoint is a replace, so that means "no group
	// grants" — same reading as an empty list. See UpdatePermissions' doc.
	return []services.GroupGrant{}, nil
}

// toGroupGrantViews converts service grants to their wire shape, always non-nil.
func toGroupGrantViews(grants []services.GroupGrant) []GroupGrantView {
	out := make([]GroupGrantView, 0, len(grants))
	for _, g := range grants {
		out = append(out, GroupGrantView{GroupID: g.GroupID, IncludeDescendants: g.IncludeDescendants})
	}
	return out
}

// Create handles POST /api/users. Self-registration is disabled (see
// AuthHandlers.Register), so this is the only way to add a user once the
// install has its first account.
func (h *UserHandlers) Create(c echo.Context) error {
	var req CreateUserRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if strings.TrimSpace(req.Username) == "" || req.Password == "" {
		return badRequest(c, "username and password are required")
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	isAdmin := false
	if req.IsAdmin != nil {
		isAdmin = *req.IsAdmin
	}
	caps := services.UserCapabilities{}
	if req.CanManageNotifications != nil {
		caps.CanManageNotifications = *req.CanManageNotifications
	}
	if req.CanManageMaintenance != nil {
		caps.CanManageMaintenance = *req.CanManageMaintenance
	}
	if req.CanCreateMonitors != nil {
		caps.CanCreateMonitors = *req.CanCreateMonitors
	}
	if req.CanCreateGroups != nil {
		caps.CanCreateGroups = *req.CanCreateGroups
	}
	user, err := h.svc.CreateUser(c.Request().Context(), req.Username, req.Password, active, isAdmin, req.Timezone, caps)
	if err != nil {
		return mapAuthError(c, err)
	}
	return c.JSON(http.StatusCreated, map[string]any{"user": toUserView(user)})
}

// List handles GET /api/users.
func (h *UserHandlers) List(c echo.Context) error {
	users, err := h.svc.ListUsers(c.Request().Context())
	if err != nil {
		return mapAuthError(c, err)
	}
	out := make([]*UserView, len(users))
	for i, u := range users {
		out[i] = toUserView(u)
	}
	return c.JSON(http.StatusOK, out)
}

// GetByID handles GET /api/users/:id.
func (h *UserHandlers) GetByID(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid id")
	}
	user, err := h.svc.GetUser(c.Request().Context(), id)
	if err != nil {
		return mapAuthError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"user": toUserView(user)})
}

// Update handles PUT /api/users/:id.
func (h *UserHandlers) Update(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid id")
	}
	var req UpdateUserRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	user, err := h.svc.UpdateUser(c.Request().Context(), id, req.Username, req.Active, req.IsAdmin, req.Timezone, req.Password,
		services.CapabilityUpdate{
			CanManageNotifications: req.CanManageNotifications,
			CanManageMaintenance:   req.CanManageMaintenance,
			CanCreateMonitors:      req.CanCreateMonitors,
			CanCreateGroups:        req.CanCreateGroups,
		})
	if err != nil {
		return mapAuthError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"user": toUserView(user)})
}

// GetPermissions handles GET /api/users/:id/permissions. Admin-only.
func (h *UserHandlers) GetPermissions(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid id")
	}
	if h.access == nil {
		return c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
	}
	// Confirm the user exists, so a typo'd id is a 404 rather than an empty set
	// that looks like "this user simply has no grants".
	if _, err := h.svc.GetUser(c.Request().Context(), id); err != nil {
		return mapAuthError(c, err)
	}

	monitorIDs, groups, err := h.access.ListPermissions(c.Request().Context(), id)
	if err != nil {
		return mapAuthError(c, err)
	}
	return c.JSON(http.StatusOK, UserPermissionsView{MonitorIDs: monitorIDs, Groups: toGroupGrantViews(groups)})
}

// UpdatePermissions handles PUT /api/users/:id/permissions. Admin-only.
//
// REPLACE, not merge: the body carries the user's complete grant set, so sending
// {"monitor_ids": [], "groups": []} revokes everything. An omitted field is an
// empty list, not "leave it alone" — a partial-update reading of this endpoint
// would make it impossible to revoke the last grant of a kind.
//
// Referencing a monitor or group that does not exist is a 400 and writes nothing.
//
// This edits VIEW grants only. It cannot give anyone the right to change a
// monitor or a group: that comes from ownership or the admin flag, neither of
// which is reachable from here. See services.AccessService.
func (h *UserHandlers) UpdatePermissions(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid id")
	}
	if h.access == nil {
		return c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
	}
	if _, err := h.svc.GetUser(c.Request().Context(), id); err != nil {
		return mapAuthError(c, err)
	}

	var req updatePermissionsRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	groups, err := req.groupGrants()
	if err != nil {
		return badRequest(c, err.Error())
	}

	if err := h.access.SetPermissions(c.Request().Context(), id, req.MonitorIDs, groups); err != nil {
		return mapAuthError(c, err)
	}

	// Read the grants back rather than echoing the request: the response then
	// reflects what was actually stored (deduplicated, and with each grant's
	// resolved reach), not what was asked for.
	monitorIDs, stored, err := h.access.ListPermissions(c.Request().Context(), id)
	if err != nil {
		return mapAuthError(c, err)
	}
	return c.JSON(http.StatusOK, UserPermissionsView{MonitorIDs: monitorIDs, Groups: toGroupGrantViews(stored)})
}

// Delete handles DELETE /api/users/:id. It guards against removing the
// last remaining user and against a caller deleting their own account —
// see AuthService.DeleteUser.
func (h *UserHandlers) Delete(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid id")
	}
	currentUserID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	if err := h.svc.DeleteUser(c.Request().Context(), currentUserID, id); err != nil {
		return mapAuthError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
