package handlers

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// errAccessDenied signals that an access check failed and the HTTP response
// has ALREADY been written by the check itself. Callers must stop immediately
// and return it; Echo's error handler skips responses that are already
// committed, so no second body is written.
//
// This sentinel exists because echo.Context.JSON returns nil on a successful
// write. Returning it directly from an access check (the previous behavior)
// meant the check reported "no error" even when it had just denied the
// request, so callers doing `if err := requireMonitorAccess(...); err != nil`
// never short-circuited and went on to perform the mutation anyway — a
// cross-tenant authorization bypass. Always signal denial with a non-nil error.
var errAccessDenied = errors.New("access denied")

// requireMonitorViewAccess ensures the authenticated user may SEE the monitor.
//
// It replaces the old requireMonitorOwnership: under RBAC "may I touch this
// monitor?" is no longer "do I own it?" — an admin sees every monitor in the
// install, and a non-admin sees the ones granted to them directly or through a
// group. services.AccessService is the only place that question is answered.
//
// Denial is reported as 404, not 403, so we never confirm the existence of a
// monitor the caller is not allowed to see.
//
// Returns nil when access is allowed. On denial it writes the error response
// and returns errAccessDenied, which is non-nil — callers MUST propagate it:
//
//	if err := requireMonitorViewAccess(c, access, id); err != nil {
//		return err
//	}
//
// NOTE: view access is NOT write access. A user may see many monitors and be
// allowed to change none of them. Routes that MUTATE an existing monitor must
// use requireMonitorEditAccess, which asks the ownership question this function
// does not. Routes that a capability holder may mutate (notification/maintenance
// links) call this to check the monitor they are pointing at, and are separately
// gated by middleware.RequireCapability.
func requireMonitorViewAccess(c echo.Context, access *services.AccessService, monitorID int64) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		_ = unauthenticated(c)
		return errAccessDenied
	}
	if access == nil {
		// Fail closed. A handler wired without an access service must not fall back
		// to "allow": that is exactly the silent-failure shape this codebase keeps
		// getting bitten by.
		_ = c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
		return errAccessDenied
	}
	allowed, err := access.CanViewMonitor(c.Request().Context(), userID, monitorID)
	if err != nil {
		_ = mapMonitorError(c, err)
		return errAccessDenied
	}
	if !allowed {
		_ = c.JSON(http.StatusNotFound, errorBody("monitor not found"))
		return errAccessDenied
	}
	return nil
}

// requireGroupViewAccess ensures the authenticated user may SEE the monitor group
// (folder). The group twin of requireMonitorViewAccess, with the same contract:
// denial is a 404 (never confirm a folder the caller may not see exists), the
// response is already written, and the returned error is non-nil and MUST be
// propagated.
func requireGroupViewAccess(c echo.Context, access *services.AccessService, groupID int64) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		_ = unauthenticated(c)
		return errAccessDenied
	}
	if access == nil {
		_ = c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
		return errAccessDenied
	}
	allowed, err := access.CanViewGroup(c.Request().Context(), userID, groupID)
	if err != nil {
		_ = c.JSON(http.StatusNotFound, errorBody("monitor group not found"))
		return errAccessDenied
	}
	if !allowed {
		_ = c.JSON(http.StatusNotFound, errorBody("monitor group not found"))
		return errAccessDenied
	}
	return nil
}

// requireMonitorEditAccess ensures the authenticated user may CHANGE the monitor
// — update, clone or delete it. It is the gate for every mutating monitor route
// that targets an EXISTING monitor.
//
// Two checks, in this order, and the order is the point:
//
//  1. view access — denied as 404, so a monitor the caller cannot see stays
//     indistinguishable from one that does not exist;
//  2. edit access (admin, or the caller created it) — denied as 403, which is
//     safe to say out loud precisely because step 1 already established that the
//     caller is allowed to know this monitor exists.
//
// Collapsing these into a single check would force one status for both cases and
// lose one of those two properties: a bare 403 leaks the existence of monitors in
// other tenants, and a bare 404 tells an owner-less-but-viewing user their
// monitor vanished.
//
// Same sentinel discipline as requireMonitorViewAccess: on denial the response is
// already written and the caller MUST propagate the returned error.
func requireMonitorEditAccess(c echo.Context, access *services.AccessService, monitorID int64) error {
	if err := requireMonitorViewAccess(c, access, monitorID); err != nil {
		return err
	}
	userID, ok := userIDFromContext(c)
	if !ok {
		_ = unauthenticated(c)
		return errAccessDenied
	}
	if access == nil {
		_ = c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
		return errAccessDenied
	}
	allowed, err := access.CanEditMonitor(c.Request().Context(), userID, monitorID)
	if err != nil {
		_ = mapMonitorError(c, err)
		return errAccessDenied
	}
	if !allowed {
		_ = c.JSON(http.StatusForbidden, errorBody("you can only modify monitors you created"))
		return errAccessDenied
	}
	return nil
}

// requireGroupEditAccess is the group twin of requireMonitorEditAccess: same
// two-step view-then-own check, same status codes, same sentinel discipline.
func requireGroupEditAccess(c echo.Context, access *services.AccessService, groupID int64) error {
	if err := requireGroupViewAccess(c, access, groupID); err != nil {
		return err
	}
	userID, ok := userIDFromContext(c)
	if !ok {
		_ = unauthenticated(c)
		return errAccessDenied
	}
	if access == nil {
		_ = c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
		return errAccessDenied
	}
	allowed, err := access.CanEditGroup(c.Request().Context(), userID, groupID)
	if err != nil {
		_ = c.JSON(http.StatusNotFound, errorBody("monitor group not found"))
		return errAccessDenied
	}
	if !allowed {
		_ = c.JSON(http.StatusForbidden, errorBody("you can only modify monitor groups you created"))
		return errAccessDenied
	}
	return nil
}

// requireAdminAccess is the in-handler admin gate for routes that stay admin-only
// no matter what capabilities a user holds — install-wide destructive actions
// like clearing heartbeat history. The router already wraps those routes in
// middleware.RequireAdmin; this is deliberate defense in depth, so that a route
// accidentally registered without the middleware still cannot be used by a
// non-admin.
//
// This is NOT the gate for mutating monitors and groups any more — those go
// through requireMonitorEditAccess / requireGroupEditAccess, which admit owners
// as well as admins. Reaching for this on such a route would lock every non-admin
// out of the monitors they created.
//
// Same sentinel discipline as requireMonitorViewAccess: on denial the response is
// already written and the caller MUST propagate the returned error.
func requireAdminAccess(c echo.Context, access *services.AccessService) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		_ = unauthenticated(c)
		return errAccessDenied
	}
	if access == nil {
		_ = c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
		return errAccessDenied
	}
	admin, err := access.IsAdmin(c.Request().Context(), userID)
	if err != nil || !admin {
		_ = c.JSON(http.StatusForbidden, errorBody("admin privileges required"))
		return errAccessDenied
	}
	return nil
}
