package handlers_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"
)

// A notification's deliveries reach what its owner may reach, so a non-admin
// notification manager may not retarget an admin-created notification (#88).
// Changing anything but the config stays open to them, and they keep full
// control of their own notifications.
func TestNotifications_NonAdminCannotRetargetAdminNotification(t *testing.T) {
	h := newNotifHTTPHarness(t)
	ctx := context.Background()

	adminNotif := h.createNotif(t, h.tokenA, "admin-hook")
	path := "/api/notifications/" + strconv.FormatInt(adminNotif, 10)
	internal := map[string]any{"url": "http://10.0.0.5/hook"}

	if rec := h.do(t, http.MethodPut, path, h.tokenB, map[string]any{"name": "admin-hook", "config": internal}); rec.Code != http.StatusForbidden {
		t.Fatalf("manager PUT new config on admin notification = %d; want 403 (body %s)", rec.Code, rec.Body.String())
	}
	stored, err := h.svc.GetByID(ctx, adminNotif)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Config["url"] != "https://hooks.example/admin-hook" {
		t.Fatalf("stored url = %v after a refused retarget", stored.Config["url"])
	}
	if rec := h.do(t, http.MethodPut, path, h.tokenB, map[string]any{
		"name": "renamed", "config": map[string]any{"url": "https://hooks.example/admin-hook"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("manager PUT rename with unchanged config = %d; want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := h.do(t, http.MethodPut, path, h.tokenA, map[string]any{"name": "renamed", "config": internal}); rec.Code != http.StatusOK {
		t.Fatalf("admin PUT new config = %d; want 200 (body %s)", rec.Code, rec.Body.String())
	}

	own := h.createNotif(t, h.tokenB, "manager-hook")
	if rec := h.do(t, http.MethodPut, "/api/notifications/"+strconv.FormatInt(own, 10), h.tokenB, map[string]any{
		"name": "manager-hook", "config": map[string]any{"url": "https://hooks.example/elsewhere"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("manager PUT config on own notification = %d; want 200 (body %s)", rec.Code, rec.Body.String())
	}
}
