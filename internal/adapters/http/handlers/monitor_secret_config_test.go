package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Issue #89: credential config values are write-only on the monitor API. Every
// role reads the placeholder; only the monitor's editors read push_token.

const (
	monSecretPassword = "basic-pass-DO-NOT-LEAK"
	monSecretHeader   = "Bearer header-DO-NOT-LEAK"
	monSecretPush     = "push-token-owner-only"
)

func seedSecretMonitors(t *testing.T, h *rbacHarness) (httpID, pushID int64) {
	t.Helper()
	m, err := h.monitors.GetByID(context.Background(), h.monitorVisible)
	if err != nil {
		t.Fatalf("load visible monitor: %v", err)
	}
	m.Config = map[string]any{
		"url":           "https://example.com/health",
		"auth_method":   "basic",
		"auth_username": "svc",
		"auth_password": monSecretPassword,
		"headers":       map[string]any{"Authorization": monSecretHeader, "Accept": "application/json"},
	}
	push := &domain.Monitor{
		UserID: h.creatorID, Name: "creator-push", Type: "push", Active: true, Interval: 60,
		GroupID: &h.groupVisible, PushToken: monSecretPush,
		Config: map[string]any{"push_token": monSecretPush},
	}
	if err := h.monitors.Create(context.Background(), push); err != nil {
		t.Fatalf("create push monitor: %v", err)
	}
	return m.ID, push.ID
}

func monitorConfigFrom(t *testing.T, body []byte, id int64) map[string]any {
	t.Helper()
	var one map[string]any
	if err := json.Unmarshal(body, &one); err == nil && one["id"] != nil {
		cfg, _ := one["config"].(map[string]any)
		return cfg
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		var wrapped struct {
			Items []map[string]any `json:"items"`
			Data  []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(body, &wrapped); err != nil {
			t.Fatalf("decode monitors: %v (%s)", err, body)
		}
		rows = append(wrapped.Items, wrapped.Data...)
	}
	for _, r := range rows {
		if v, _ := r["id"].(float64); int64(v) == id {
			cfg, _ := r["config"].(map[string]any)
			return cfg
		}
	}
	t.Fatalf("monitor %d not in response: %s", id, body)
	return nil
}

func TestMonitorSecrets_MaskedForEveryRole(t *testing.T) {
	h := newRBACHarness(t)
	httpID, pushID := seedSecretMonitors(t, h)

	roles := []struct {
		name, token  string
		seesPushTokn bool // may edit the push monitor (admin or its creator)
	}{
		{"viewer", h.memberToken, false},
		{"view-all", h.viewAllToken, false},
		{"creator", h.creatorToken, true},
		{"admin", h.adminToken, true},
	}
	for _, role := range roles {
		t.Run(role.name, func(t *testing.T) {
			for _, path := range []string{"/api/monitors", "/api/monitors/" + strconv.FormatInt(httpID, 10)} {
				rec := h.do(t, http.MethodGet, path, role.token, nil)
				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s = %d (%s)", path, rec.Code, rec.Body.String())
				}
				body := rec.Body.String()
				for _, secret := range []string{monSecretPassword, monSecretHeader} {
					if strings.Contains(body, secret) {
						t.Fatalf("GET %s leaked %q to %s", path, secret, role.name)
					}
				}
				cfg := monitorConfigFrom(t, rec.Body.Bytes(), httpID)
				if cfg["auth_password"] != domain.SecretConfigPlaceholder {
					t.Errorf("GET %s auth_password = %v; want placeholder", path, cfg["auth_password"])
				}
				if hdr := cfg["headers"].(map[string]any); hdr["Accept"] != "application/json" {
					t.Errorf("GET %s ordinary header lost: %v", path, hdr)
				}
			}
			for _, path := range []string{"/api/monitors", "/api/monitors/" + strconv.FormatInt(pushID, 10)} {
				rec := h.do(t, http.MethodGet, path, role.token, nil)
				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s = %d", path, rec.Code)
				}
				got := monitorConfigFrom(t, rec.Body.Bytes(), pushID)["push_token"]
				want := any(domain.SecretConfigPlaceholder)
				if role.seesPushTokn {
					want = monSecretPush
				}
				if got != want {
					t.Errorf("GET %s push_token for %s = %v; want %v", path, role.name, got, want)
				}
			}
		})
	}
}

func TestMonitorSecrets_PutPlaceholderKeepsStoredSecret(t *testing.T) {
	h := newRBACHarness(t)
	httpID, _ := seedSecretMonitors(t, h)
	path := "/api/monitors/" + strconv.FormatInt(httpID, 10)

	rec := h.do(t, http.MethodGet, path, h.adminToken, nil)
	cfg := monitorConfigFrom(t, rec.Body.Bytes(), httpID)
	cfg["auth_username"] = "svc2" // an ordinary edit alongside the masked values

	rec = h.do(t, http.MethodPut, path, h.adminToken, map[string]any{"config": cfg})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", rec.Code, rec.Body.String())
	}
	stored, _ := h.monitors.GetByID(context.Background(), httpID)
	if stored.Config["auth_password"] != monSecretPassword {
		t.Fatalf("stored auth_password = %v; the placeholder overwrote the secret", stored.Config["auth_password"])
	}
	if stored.Config["headers"].(map[string]any)["Authorization"] != monSecretHeader {
		t.Fatalf("stored Authorization header lost: %v", stored.Config["headers"])
	}
	if stored.Config["auth_username"] != "svc2" {
		t.Fatalf("ordinary edit not applied: %v", stored.Config["auth_username"])
	}
	if strings.Contains(rec.Body.String(), monSecretPassword) {
		t.Fatal("PUT response leaked the secret")
	}
}

func TestMonitorSecrets_PutNewValueReplacesSecret(t *testing.T) {
	h := newRBACHarness(t)
	httpID, _ := seedSecretMonitors(t, h)
	path := "/api/monitors/" + strconv.FormatInt(httpID, 10)

	rec := h.do(t, http.MethodGet, path, h.adminToken, nil)
	cfg := monitorConfigFrom(t, rec.Body.Bytes(), httpID)
	cfg["auth_password"] = "rotated-password"
	rec = h.do(t, http.MethodPut, path, h.adminToken, map[string]any{"config": cfg})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", rec.Code, rec.Body.String())
	}
	stored, _ := h.monitors.GetByID(context.Background(), httpID)
	if stored.Config["auth_password"] != "rotated-password" {
		t.Fatalf("stored auth_password = %v; want rotated value", stored.Config["auth_password"])
	}
}

func TestMonitorSecrets_CreateRejectsPlaceholder(t *testing.T) {
	h := newRBACHarness(t)
	rec := h.do(t, http.MethodPost, "/api/monitors", h.adminToken, map[string]any{
		"name": "copy", "type": "http", "interval": 60,
		"config": map[string]any{"url": "https://example.com", "auth_password": domain.SecretConfigPlaceholder},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST with placeholder = %d; want 400 (%s)", rec.Code, rec.Body.String())
	}
}
