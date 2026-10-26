package handlers_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Docker monitors reach a Docker daemon (the local socket or a chosen
// docker_daemon address), so only admins may create, clone or reconfigure
// them, even when a non-admin holds create_monitors (#88).
func TestRBAC_DockerMonitorsAreAdminOnly(t *testing.T) {
	h := newRBACHarness(t)
	docker := func(name string) map[string]any {
		return map[string]any{
			"name":     name,
			"type":     "docker",
			"group_id": h.groupCreator,
			"config":   map[string]any{"container": "web", "docker_daemon": "tcp://10.0.0.5:2375"},
		}
	}

	if rec := h.do(t, http.MethodPost, "/api/monitors", h.creatorToken, docker("creator-docker")); rec.Code != http.StatusForbidden {
		t.Fatalf("creator POST docker monitor = %d; want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := h.do(t, http.MethodPost, "/api/monitors", h.adminToken, docker("admin-docker")); rec.Code != http.StatusCreated {
		t.Fatalf("admin POST docker monitor = %d; want 201 (body %s)", rec.Code, rec.Body.String())
	}

	// A docker monitor a non-admin created before this rule (or was handed by
	// an admin) cannot be reconfigured or cloned by them.
	legacy := &domain.Monitor{UserID: h.creatorID, Name: "legacy-docker", Type: "docker", Active: true, Interval: 60,
		GroupID: &h.groupCreator, Config: map[string]any{"container": "web"}}
	if err := h.monitors.Create(context.Background(), legacy); err != nil {
		t.Fatalf("seed legacy docker monitor: %v", err)
	}
	path := "/api/monitors/" + strconv.FormatInt(legacy.ID, 10)
	update := map[string]any{"config": map[string]any{"container": "web", "docker_daemon": "tcp://169.254.169.254:80"}}
	if rec := h.do(t, http.MethodPut, path, h.creatorToken, update); rec.Code != http.StatusForbidden {
		t.Fatalf("creator PUT docker config = %d; want 403 (body %s)", rec.Code, rec.Body.String())
	}
	// Pause/resume without a config change stays with the owner.
	if rec := h.do(t, http.MethodPut, path, h.creatorToken, map[string]any{"active": false}); rec.Code != http.StatusOK {
		t.Fatalf("creator PUT active=false on own docker monitor = %d; want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := h.do(t, http.MethodPost, path+"/clone", h.creatorToken, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("creator clone docker monitor = %d; want 403 (body %s)", rec.Code, rec.Body.String())
	}

	// Other types are unaffected for the same user.
	if rec := h.do(t, http.MethodPost, "/api/monitors", h.creatorToken, map[string]any{
		"name": "creator-http", "type": "http", "group_id": h.groupCreator,
		"config": map[string]any{"url": "https://example.com"},
	}); rec.Code != http.StatusCreated {
		t.Fatalf("creator POST http monitor = %d; want 201 (body %s)", rec.Code, rec.Body.String())
	}
}
