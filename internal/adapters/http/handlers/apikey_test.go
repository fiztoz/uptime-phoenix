package handlers_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
)

// createAPIKeyResponse mirrors POST /api/api-keys JSON (snake_case APIKeyView).
type createAPIKeyResponse struct {
	Key    string `json:"key"`
	APIKey struct {
		ID     int64  `json:"id"`
		UserID int64  `json:"user_id"`
		Name   string `json:"name"`
		Active bool   `json:"active"`
	} `json:"api_key"`
}

type apiKeyListItem struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

type meResponse struct {
	User struct {
		ID int64 `json:"id"`
	} `json:"user"`
}

// TestAPIKeyHandlers_CreateListDelete_Ownership: POST must not 500, created key
// belongs to /api/auth/me user, DELETE succeeds, GET is empty.
func TestAPIKeyHandlers_CreateListDelete_Ownership(t *testing.T) {
	h := newHarness(t)

	reg := h.do(t, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "apikey_user",
		"password": "supersecret",
	}, "")
	if reg.Code != http.StatusCreated {
		t.Fatalf("register status = %d; want 201; body=%s", reg.Code, reg.Body.String())
	}
	var login handlers.LoginResponse
	if err := json.Unmarshal(reg.Body.Bytes(), &login); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	if login.Token == "" {
		t.Fatal("register returned no token")
	}

	meRec := h.do(t, http.MethodGet, "/api/auth/me", nil, login.Token)
	if meRec.Code != http.StatusOK {
		t.Fatalf("me status = %d; want 200; body=%s", meRec.Code, meRec.Body.String())
	}
	var me meResponse
	if err := json.Unmarshal(meRec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	if me.User.ID == 0 {
		t.Fatal("me returned zero user id")
	}

	createRec := h.do(t, http.MethodPost, "/api/api-keys", map[string]any{
		"name":   "smoke-key",
		"scopes": []string{"read", "write"},
	}, login.Token)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d; want 201 (not 500); body=%s", createRec.Code, createRec.Body.String())
	}
	var created createAPIKeyResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Key == "" {
		t.Error("create response missing plaintext key")
	}
	if created.APIKey.ID == 0 {
		t.Fatal("create response missing api_key.id")
	}
	if created.APIKey.UserID != me.User.ID {
		t.Errorf("api_key.user_id = %d; want %d from /api/auth/me", created.APIKey.UserID, me.User.ID)
	}

	listAfterCreate := h.do(t, http.MethodGet, "/api/api-keys", nil, login.Token)
	if listAfterCreate.Code != http.StatusOK {
		t.Fatalf("list after create status = %d; want 200; body=%s", listAfterCreate.Code, listAfterCreate.Body.String())
	}
	var keysAfterCreate []apiKeyListItem
	if err := json.Unmarshal(listAfterCreate.Body.Bytes(), &keysAfterCreate); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(keysAfterCreate) != 1 {
		t.Fatalf("list after create len = %d; want 1; body=%s", len(keysAfterCreate), listAfterCreate.Body.String())
	}
	if keysAfterCreate[0].ID != created.APIKey.ID {
		t.Errorf("list[0].id = %d; want %d", keysAfterCreate[0].ID, created.APIKey.ID)
	}
	if keysAfterCreate[0].CreatedAt == "" {
		t.Error("list[0].created_at is empty")
	}

	deleteRec := h.do(t, http.MethodDelete, "/api/api-keys/"+strconv.FormatInt(created.APIKey.ID, 10), nil, login.Token)
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d; want 204; body=%s", deleteRec.Code, deleteRec.Body.String())
	}

	listAfterDelete := h.do(t, http.MethodGet, "/api/api-keys", nil, login.Token)
	if listAfterDelete.Code != http.StatusOK {
		t.Fatalf("list after delete status = %d; want 200; body=%s", listAfterDelete.Code, listAfterDelete.Body.String())
	}
	if listAfterDelete.Body.String() != "[]\n" && listAfterDelete.Body.String() != "[]" {
		t.Errorf("list after delete = %s; want []", listAfterDelete.Body.String())
	}
}

func TestAPIKeyHandlers_Create_Unauthorized(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, http.MethodPost, "/api/api-keys", map[string]string{"name": "nope"}, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("create without token status = %d; want 401", rec.Code)
	}
}
