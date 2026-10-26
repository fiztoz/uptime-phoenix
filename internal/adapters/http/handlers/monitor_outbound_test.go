package handlers_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// adminUserID is the first registered user in the RBAC and notification
// harnesses, which is always an admin.
const adminUserID int64 = 1

// Database monitors connect through engine drivers the outbound guard does
// not cover, so they are admin-only like docker (#88).
func TestRBAC_DatabaseMonitorsAreAdminOnly(t *testing.T) {
	h := newRBACHarness(t)
	db := func(name string) map[string]any {
		return map[string]any{"name": name, "type": "database", "group_id": h.groupCreator,
			"config": map[string]any{"engine": "postgres", "hostname": "10.0.0.5", "port": 5432.0, "health_check": "ping"}}
	}
	if rec := h.do(t, http.MethodPost, "/api/monitors", h.creatorToken, db("creator-db")); rec.Code != http.StatusForbidden {
		t.Fatalf("creator POST database monitor = %d; want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := h.do(t, http.MethodPost, "/api/monitors", h.adminToken, db("admin-db")); rec.Code == http.StatusForbidden {
		t.Fatalf("admin POST database monitor = 403; want allowed (body %s)", rec.Body.String())
	}
}

// Only admins assign proxies; a non-admin may keep or clear one an admin
// assigned, but not choose one (#88).
func TestRBAC_ProxyAssignmentIsAdminOnly(t *testing.T) {
	h := newRBACHarness(t)
	ctx := context.Background()
	body := func(name string, proxyID any) map[string]any {
		return map[string]any{"name": name, "type": "http", "group_id": h.groupCreator,
			"config": map[string]any{"url": "https://example.com"}, "proxy_id": proxyID}
	}

	if rec := h.do(t, http.MethodPost, "/api/monitors", h.creatorToken, body("creator-proxy", 7)); rec.Code != http.StatusForbidden {
		t.Fatalf("creator POST with proxy_id = %d; want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := h.do(t, http.MethodPost, "/api/monitors", h.creatorToken, body("creator-direct", nil)); rec.Code != http.StatusCreated {
		t.Fatalf("creator POST without proxy = %d; want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := h.do(t, http.MethodPost, "/api/monitors", h.adminToken, body("admin-proxy", 7)); rec.Code == http.StatusForbidden {
		t.Fatalf("admin POST with proxy_id = 403; want past the access gate (body %s)", rec.Body.String())
	}

	// A creator-owned monitor an admin attached proxy 7 to.
	proxyID := int64(7)
	own := &domain.Monitor{UserID: h.creatorID, Name: "own", Type: "http", Active: true, Interval: 60,
		GroupID: &h.groupCreator, ProxyID: &proxyID, Config: map[string]any{"url": "https://example.com"}}
	if err := h.monitors.Create(ctx, own); err != nil {
		t.Fatal(err)
	}
	path := "/api/monitors/" + strconv.FormatInt(own.ID, 10)
	if rec := h.do(t, http.MethodPut, path, h.creatorToken, map[string]any{"proxy_id": 8}); rec.Code != http.StatusForbidden {
		t.Fatalf("creator PUT proxy_id change = %d; want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if stored, _ := h.monitors.GetByID(ctx, own.ID); stored.ProxyID == nil || *stored.ProxyID != 7 {
		t.Fatalf("stored proxy_id = %v after a refused change; want 7", stored.ProxyID)
	}
	// The form echoes the stored proxy on every save; that must keep passing
	// the access gate (this harness has no proxy store, so the service then
	// answers 400 for the proxy itself).
	if rec := h.do(t, http.MethodPut, path, h.creatorToken, map[string]any{"name": "renamed", "proxy_id": 7}); rec.Code == http.StatusForbidden {
		t.Fatalf("creator PUT with unchanged proxy_id = 403; want past the access gate (body %s)", rec.Body.String())
	}
	if rec := h.do(t, http.MethodPut, path, h.creatorToken, map[string]any{"proxy_id": nil}); rec.Code != http.StatusOK {
		t.Fatalf("creator PUT clearing proxy_id = %d; want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := h.do(t, http.MethodPut, path, h.adminToken, map[string]any{"proxy_id": 9}); rec.Code == http.StatusForbidden {
		t.Fatalf("admin PUT proxy_id = 403; want past the access gate (body %s)", rec.Body.String())
	}

	// Cloning a proxied monitor would hand the clone (owned by the caller)
	// the proxy, so a non-admin cannot.
	shared := &domain.Monitor{UserID: adminUserID, Name: "shared-proxied", Type: "http", Active: true, Interval: 60,
		GroupID: &h.groupCreator, ProxyID: &proxyID, Config: map[string]any{"url": "https://example.com"}}
	if err := h.monitors.Create(ctx, shared); err != nil {
		t.Fatal(err)
	}
	h.access.InvalidateUser(h.creatorID) // the cached view predates the seeded monitor
	if rec := h.do(t, http.MethodPost, "/api/monitors/"+strconv.FormatInt(shared.ID, 10)+"/clone", h.creatorToken, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("creator clone of a proxied monitor = %d; want 403 (body %s)", rec.Code, rec.Body.String())
	}
}

// save_body_on_* is limited to admin-owned monitors, whoever makes the
// request (#88).
func TestRBAC_SaveBodyIsLimitedToAdminOwnedMonitors(t *testing.T) {
	h := newRBACHarness(t)
	ctx := context.Background()
	saveBody := map[string]any{"url": "https://example.com", "save_body_on_error": true}

	if rec := h.do(t, http.MethodPost, "/api/monitors", h.creatorToken, map[string]any{
		"name": "creator-body", "type": "http", "group_id": h.groupCreator, "config": saveBody,
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("creator POST with save_body_on_error = %d; want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := h.do(t, http.MethodPost, "/api/monitors", h.adminToken, map[string]any{
		"name": "admin-body", "type": "http", "group_id": h.groupCreator,
		"config": map[string]any{"url": "https://example.com", "save_body_on_success": true},
	}); rec.Code != http.StatusCreated {
		t.Fatalf("admin POST with save_body_on_success = %d; want 201 (body %s)", rec.Code, rec.Body.String())
	}

	own := &domain.Monitor{UserID: h.creatorID, Name: "own", Type: "http", Active: true, Interval: 60,
		GroupID: &h.groupCreator, Config: map[string]any{"url": "https://example.com"}}
	if err := h.monitors.Create(ctx, own); err != nil {
		t.Fatal(err)
	}
	path := "/api/monitors/" + strconv.FormatInt(own.ID, 10)
	for name, token := range map[string]string{"creator": h.creatorToken, "admin": h.adminToken} {
		if rec := h.do(t, http.MethodPut, path, token, map[string]any{"config": saveBody}); rec.Code != http.StatusForbidden {
			t.Fatalf("%s PUT save_body on a creator-owned monitor = %d; want 403 (body %s)", name, rec.Code, rec.Body.String())
		}
	}
	if stored, _ := h.monitors.GetByID(ctx, own.ID); configBool(stored.Config, "save_body_on_error") {
		t.Fatal("save_body_on_error was stored despite the 403")
	}

	shared := &domain.Monitor{UserID: adminUserID, Name: "shared-body", Type: "http", Active: true, Interval: 60,
		GroupID: &h.groupCreator, Config: saveBody}
	if err := h.monitors.Create(ctx, shared); err != nil {
		t.Fatal(err)
	}
	h.access.InvalidateUser(h.creatorID) // the cached view predates the seeded monitor
	if rec := h.do(t, http.MethodPost, "/api/monitors/"+strconv.FormatInt(shared.ID, 10)+"/clone", h.creatorToken, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("creator clone of a save_body monitor = %d; want 403 (body %s)", rec.Code, rec.Body.String())
	}
}

func configBool(cfg map[string]any, key string) bool {
	v, _ := cfg[key].(bool)
	return v
}
