package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Issue #89: notification delivery URLs and tokens are write-only for every
// role, in list and detail responses.

const notifSecretURL = "https://hooks.example/services/T0K3N-DO-NOT-LEAK"

func seedSecretNotification(t *testing.T, h *notifHTTPHarness) int64 {
	t.Helper()
	rec := h.do(t, http.MethodPost, "/api/notifications", h.tokenA, map[string]any{
		"name": "secret-hook", "type": "webhook",
		"config": map[string]any{"url": notifSecretURL, "secret": "hmac-DO-NOT-LEAK", "method": "POST"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "DO-NOT-LEAK") {
		t.Fatalf("create response leaked a secret: %s", rec.Body.String())
	}
	var v map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	id := int64(v["id"].(float64))
	// Attach to monitor B so the no-capability viewer (C) can see it.
	if rec := h.do(t, http.MethodPost,
		"/api/notifications/"+floatToIntStr(float64(id))+"/monitor/"+floatToIntStr(float64(h.monitorBID)),
		h.tokenA, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("attach: %d (%s)", rec.Code, rec.Body.String())
	}
	return id
}

func TestNotificationSecrets_MaskedForEveryRole(t *testing.T) {
	h := newNotifHTTPHarness(t)
	id := seedSecretNotification(t, h)
	idStr := floatToIntStr(float64(id))

	for _, role := range []struct{ name, token string }{
		{"viewer", h.tokenC}, {"view-all", h.tokenD}, {"manager", h.tokenB}, {"admin", h.tokenA},
	} {
		t.Run(role.name, func(t *testing.T) {
			list := h.do(t, http.MethodGet, "/api/notifications", role.token, nil)
			if list.Code != http.StatusOK {
				t.Fatalf("list = %d", list.Code)
			}
			var rows []map[string]any
			if err := json.Unmarshal(list.Body.Bytes(), &rows); err != nil {
				t.Fatalf("decode list: %v", err)
			}
			var found map[string]any
			for _, r := range rows {
				if int64(r["id"].(float64)) == id {
					found = r
				}
			}
			if found == nil {
				t.Fatalf("%s cannot see the seeded notification; test would prove nothing", role.name)
			}
			get := h.do(t, http.MethodGet, "/api/notifications/"+idStr, role.token, nil)
			if get.Code != http.StatusOK {
				t.Fatalf("get = %d", get.Code)
			}
			var one map[string]any
			_ = json.Unmarshal(get.Body.Bytes(), &one)
			for name, body := range map[string]string{"list": list.Body.String(), "get": get.Body.String()} {
				if strings.Contains(body, "DO-NOT-LEAK") {
					t.Fatalf("%s leaked a secret to %s", name, role.name)
				}
			}
			for name, v := range map[string]map[string]any{"list": found, "get": one} {
				cfg := v["config"].(map[string]any)
				if cfg["url"] != domain.SecretConfigPlaceholder || cfg["secret"] != domain.SecretConfigPlaceholder {
					t.Errorf("%s config = %v; want masked url and secret", name, cfg)
				}
				if cfg["method"] != "POST" {
					t.Errorf("%s ordinary value lost: %v", name, cfg)
				}
			}
		})
	}
}

func TestNotificationSecrets_PutPlaceholderKeepsAndNewValueReplaces(t *testing.T) {
	h := newNotifHTTPHarness(t)
	ctx := context.Background()
	id := seedSecretNotification(t, h)
	path := "/api/notifications/" + floatToIntStr(float64(id))

	var view map[string]any
	_ = json.Unmarshal(h.do(t, http.MethodGet, path, h.tokenB, nil).Body.Bytes(), &view)
	cfg := view["config"].(map[string]any)
	cfg["method"] = "PUT"
	if rec := h.do(t, http.MethodPut, path, h.tokenB, map[string]any{"config": cfg}); rec.Code != http.StatusOK {
		t.Fatalf("PUT masked = %d (%s)", rec.Code, rec.Body.String())
	}
	stored, err := h.svc.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Config["url"] != notifSecretURL || stored.Config["secret"] != "hmac-DO-NOT-LEAK" {
		t.Fatalf("placeholder overwrote stored secrets: %v", stored.Config)
	}
	if stored.Config["method"] != "PUT" {
		t.Fatalf("ordinary edit lost: %v", stored.Config)
	}

	cfg["url"] = "https://hooks.example/rotated"
	if rec := h.do(t, http.MethodPut, path, h.tokenB, map[string]any{"config": cfg}); rec.Code != http.StatusOK {
		t.Fatalf("PUT new = %d (%s)", rec.Code, rec.Body.String())
	}
	stored, _ = h.svc.GetByID(ctx, id)
	if stored.Config["url"] != "https://hooks.example/rotated" {
		t.Fatalf("new url not applied: %v", stored.Config["url"])
	}
}

func TestNotificationSecrets_CreateRejectsPlaceholder(t *testing.T) {
	h := newNotifHTTPHarness(t)
	rec := h.do(t, http.MethodPost, "/api/notifications", h.tokenA, map[string]any{
		"name": "copy", "type": "webhook",
		"config": map[string]any{"url": domain.SecretConfigPlaceholder},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST with placeholder = %d; want 400", rec.Code)
	}
}
