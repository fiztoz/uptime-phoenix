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

// A push monitor's hmac_secret is the key handlers/push.go verifies
// X-Signature with. It is write-only like push_token's siblings: masked for
// every reader, and a PUT that sends the mask back keeps the stored key.
func TestMonitorSecrets_PushHMACSecretMaskedAndRestored(t *testing.T) {
	h := newRBACHarness(t)
	const hmacKey = "hmac-key-DO-NOT-LEAK"
	push := &domain.Monitor{
		UserID: h.creatorID, Name: "signed-push", Type: "push", Active: true, Interval: 60,
		GroupID: &h.groupVisible, PushToken: "push-token-hmac-test",
		Config: map[string]any{"push_token": "push-token-hmac-test", "hmac_secret": hmacKey},
	}
	if err := h.monitors.Create(context.Background(), push); err != nil {
		t.Fatalf("create push monitor: %v", err)
	}
	path := "/api/monitors/" + strconv.FormatInt(push.ID, 10)

	rec := h.do(t, http.MethodGet, path, h.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), hmacKey) {
		t.Fatalf("GET leaked hmac_secret: %s", rec.Body.String())
	}
	cfg := monitorConfigFrom(t, rec.Body.Bytes(), push.ID)
	if cfg["hmac_secret"] != domain.SecretConfigPlaceholder {
		t.Fatalf("hmac_secret = %v; want the placeholder", cfg["hmac_secret"])
	}

	if rec := h.do(t, http.MethodPut, path, h.adminToken, map[string]any{"config": cfg}); rec.Code != http.StatusOK {
		t.Fatalf("PUT masked = %d (%s)", rec.Code, rec.Body.String())
	}
	stored, _ := h.monitors.GetByID(context.Background(), push.ID)
	if stored.Config["hmac_secret"] != hmacKey {
		t.Fatalf("stored hmac_secret = %v; the placeholder overwrote the key", stored.Config["hmac_secret"])
	}

	cfg["hmac_secret"] = "rotated-hmac-key"
	if rec := h.do(t, http.MethodPut, path, h.adminToken, map[string]any{"config": cfg}); rec.Code != http.StatusOK {
		t.Fatalf("PUT new = %d (%s)", rec.Code, rec.Body.String())
	}
	stored, _ = h.monitors.GetByID(context.Background(), push.ID)
	if stored.Config["hmac_secret"] != "rotated-hmac-key" {
		t.Fatalf("new hmac_secret not applied: %v", stored.Config["hmac_secret"])
	}
}

// A masked list entry that cannot be matched to one stored entry (the list
// changed length and the masked forms are identical) must be refused, not
// restored by index and not persisted as the literal marker.
func TestMonitorSecrets_PutRefusesUnmatchableListPlaceholder(t *testing.T) {
	h := newRBACHarness(t)
	httpID, _ := seedSecretMonitors(t, h)
	m, _ := h.monitors.GetByID(context.Background(), httpID)
	m.Config["accounts"] = []any{
		map[string]any{"password": "p1-DO-NOT-LEAK"},
		map[string]any{"password": "p2-DO-NOT-LEAK"},
	}
	if err := h.monitors.Update(context.Background(), m); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
	path := "/api/monitors/" + strconv.FormatInt(httpID, 10)
	cfg := monitorConfigFrom(t, h.do(t, http.MethodGet, path, h.adminToken, nil).Body.Bytes(), httpID)
	cfg["accounts"] = cfg["accounts"].([]any)[:1] // drop one; which one is unknowable

	rec := h.do(t, http.MethodPut, path, h.adminToken, map[string]any{"config": cfg})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT = %d (%s); want 400 for an unmatchable placeholder", rec.Code, rec.Body.String())
	}
	stored, _ := h.monitors.GetByID(context.Background(), httpID)
	if got := len(stored.Config["accounts"].([]any)); got != 2 {
		t.Fatalf("stored accounts = %d entries; the refused update was applied", got)
	}
}

// The notification form saves webhook headers as a JSON string, which the
// map-only header masking did not see: the receiver's Authorization went out
// in every GET. The string is masked as a whole and restored as a whole.
func TestNotificationSecrets_WebhookHeadersStringMaskedAndRestored(t *testing.T) {
	h := newNotifHTTPHarness(t)
	ctx := context.Background()
	const headers = `{"Authorization": "Bearer hdr-DO-NOT-LEAK", "X-Team": "ops"}`
	rec := h.do(t, http.MethodPost, "/api/notifications", h.tokenA, map[string]any{
		"name": "hdr-hook", "type": "webhook",
		"config": map[string]any{"url": "https://hooks.example/x", "headers": headers},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "DO-NOT-LEAK") {
		t.Fatalf("create response leaked the header: %s", rec.Body.String())
	}
	var v map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	id := int64(v["id"].(float64))
	path := "/api/notifications/" + floatToIntStr(float64(id))

	got := h.do(t, http.MethodGet, path, h.tokenA, nil)
	if strings.Contains(got.Body.String(), "DO-NOT-LEAK") {
		t.Fatalf("GET leaked the header: %s", got.Body.String())
	}
	var view map[string]any
	_ = json.Unmarshal(got.Body.Bytes(), &view)
	cfg := view["config"].(map[string]any)
	if cfg["headers"] != domain.SecretConfigPlaceholder {
		t.Fatalf("headers = %v; want the placeholder", cfg["headers"])
	}
	cfg["method"] = "PUT"
	if rec := h.do(t, http.MethodPut, path, h.tokenA, map[string]any{"config": cfg}); rec.Code != http.StatusOK {
		t.Fatalf("PUT masked = %d (%s)", rec.Code, rec.Body.String())
	}
	stored, err := h.svc.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Config["headers"] != headers {
		t.Fatalf("stored headers = %v; the placeholder overwrote them", stored.Config["headers"])
	}
}
