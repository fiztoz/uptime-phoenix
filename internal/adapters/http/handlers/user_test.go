package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/middleware"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// userHarness wires the admin user-management endpoints (/api/users)
// behind middleware.SessionOrAPIKey + middleware.RequireAdmin, mirroring
// the production router's wiring in router.go.
type userHarness struct {
	router  *echo.Echo
	svc     *services.AuthService
	users   *memory.UserRepo
	perms   *memory.UserPermissionRepo
	access  *services.AccessService
	groups  *fakeMonitorGroupRepo
	monitor *fakeMonitorRepo
}

func newUserHarness(t *testing.T) *userHarness {
	t.Helper()

	userRepo := memory.NewUserRepo()
	apiKeyRepo := memory.NewAPIKeyRepo()
	permRepo := memory.NewUserPermissionRepo()
	groupRepo := newFakeMonitorGroupRepo()
	monitorRepo := newFakeMonitorRepo()
	authenticator := auth.NewJWTAuthenticator("test-signing-key-do-not-use-in-prod", 24, userRepo)
	totp := auth.NewTOTPProvider("Phoenix")
	svc := services.NewAuthService(userRepo, apiKeyRepo, authenticator, totp)
	accessSvc := services.NewAccessService(userRepo, permRepo, groupRepo, monitorRepo)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	userH := handlers.NewUserHandlers(svc, accessSvc)
	userGroup := e.Group("/api/users",
		middleware.SessionOrAPIKey(svc, apiKeyRepo, "write"),
		middleware.RequireAdmin(svc),
	)
	userGroup.POST("", userH.Create)
	userGroup.GET("", userH.List)
	userGroup.GET("/:id", userH.GetByID)
	userGroup.PUT("/:id", userH.Update)
	userGroup.DELETE("/:id", userH.Delete)
	userGroup.GET("/:id/permissions", userH.GetPermissions)
	userGroup.PUT("/:id/permissions", userH.UpdatePermissions)

	return &userHarness{
		router:  e,
		svc:     svc,
		users:   userRepo,
		perms:   permRepo,
		access:  accessSvc,
		groups:  groupRepo,
		monitor: monitorRepo,
	}
}

func (h *userHarness) doWithHeaders(t *testing.T, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func (h *userHarness) doWithToken(t *testing.T, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{}
	if token != "" {
		headers[echo.HeaderAuthorization] = "Bearer " + token
	}
	return h.doWithHeaders(t, method, path, body, headers)
}

// bootstrapAdmin creates the first user (Register always makes the first
// user an admin) and returns its ID and a session token for it.
func (h *userHarness) bootstrapAdmin(t *testing.T) (int64, string) {
	t.Helper()
	const password = "supersecret"
	ctx := context.Background()
	u, err := h.svc.Register(ctx, "alice", password)
	if err != nil {
		t.Fatalf("bootstrap register: %v", err)
	}
	token, err := h.svc.Login(ctx, "alice", password)
	if err != nil {
		t.Fatalf("bootstrap login: %v", err)
	}
	return u.ID, token
}

// --- tests ----------------------------------------------------------------

func TestUserHandlers_Create_Success(t *testing.T) {
	h := newUserHarness(t)
	_, adminToken := h.bootstrapAdmin(t)

	rec := h.doWithToken(t, http.MethodPost, "/api/users", map[string]any{
		"username": "bob",
		"password": "supersecret",
	}, adminToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		User handlers.UserView `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.User.Username != "bob" {
		t.Errorf("Username = %q; want bob", resp.User.Username)
	}
	if !resp.User.Active {
		t.Errorf("Active = false; want true (default)")
	}
	if resp.User.IsAdmin {
		t.Errorf("IsAdmin = true; want false (default)")
	}
	if resp.User.Timezone != "UTC" {
		t.Errorf("Timezone = %q; want UTC (default)", resp.User.Timezone)
	}
}

func TestUserHandlers_Create_Duplicate(t *testing.T) {
	h := newUserHarness(t)
	_, adminToken := h.bootstrapAdmin(t)

	body := map[string]any{"username": "bob", "password": "supersecret"}
	first := h.doWithToken(t, http.MethodPost, "/api/users", body, adminToken)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status = %d; want 201; body=%s", first.Code, first.Body.String())
	}
	rec := h.doWithToken(t, http.MethodPost, "/api/users", body, adminToken)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409", rec.Code)
	}
}

func TestUserHandlers_Create_ShortPassword(t *testing.T) {
	h := newUserHarness(t)
	_, adminToken := h.bootstrapAdmin(t)

	rec := h.doWithToken(t, http.MethodPost, "/api/users", map[string]any{
		"username": "bob",
		"password": "short",
	}, adminToken)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUserHandlers_List(t *testing.T) {
	h := newUserHarness(t)
	_, adminToken := h.bootstrapAdmin(t)
	_ = h.doWithToken(t, http.MethodPost, "/api/users", map[string]any{
		"username": "bob",
		"password": "supersecret",
	}, adminToken)

	rec := h.doWithToken(t, http.MethodGet, "/api/users", nil, adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	var users []handlers.UserView
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("len(users) = %d; want 2", len(users))
	}
}

func TestUserHandlers_Delete_Self_Conflict(t *testing.T) {
	h := newUserHarness(t)
	adminID, adminToken := h.bootstrapAdmin(t)
	// A second user so the last-user guard does not also apply here.
	_ = h.doWithToken(t, http.MethodPost, "/api/users", map[string]any{
		"username": "bob",
		"password": "supersecret",
	}, adminToken)

	rec := h.doWithToken(t, http.MethodDelete, "/api/users/"+strconv.FormatInt(adminID, 10), nil, adminToken)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["error"] != "cannot delete your own account" {
		t.Errorf("error = %q; want %q", resp["error"], "cannot delete your own account")
	}
}

func TestUserHandlers_Delete_Success(t *testing.T) {
	h := newUserHarness(t)
	_, adminToken := h.bootstrapAdmin(t)
	createRec := h.doWithToken(t, http.MethodPost, "/api/users", map[string]any{
		"username": "bob",
		"password": "supersecret",
	}, adminToken)
	var created struct {
		User handlers.UserView `json:"user"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rec := h.doWithToken(t, http.MethodDelete, "/api/users/"+strconv.FormatInt(created.User.ID, 10), nil, adminToken)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d; want 204; body=%s", rec.Code, rec.Body.String())
	}
}

// TestUserHandlers_RequireAdmin_NonAdminRejected verifies a valid session
// belonging to a non-admin user is authenticated (passes SessionOrAPIKey)
// but rejected by RequireAdmin with 403.
func TestUserHandlers_RequireAdmin_NonAdminRejected(t *testing.T) {
	h := newUserHarness(t)
	_, adminToken := h.bootstrapAdmin(t)
	_ = h.doWithToken(t, http.MethodPost, "/api/users", map[string]any{
		"username": "bob",
		"password": "supersecret",
	}, adminToken)

	ctx := context.Background()
	bobToken, err := h.svc.Login(ctx, "bob", "supersecret")
	if err != nil {
		t.Fatalf("login as bob: %v", err)
	}

	rec := h.doWithToken(t, http.MethodGet, "/api/users", nil, bobToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["error"] != "admin privileges required" {
		t.Errorf("error = %q; want %q", resp["error"], "admin privileges required")
	}
}

// TestUserHandlers_APIKeyAuth_WriteScope_Success verifies an API key
// belonging to an admin, carrying the "write" scope, can reach /api/users.
func TestUserHandlers_APIKeyAuth_WriteScope_Success(t *testing.T) {
	h := newUserHarness(t)
	adminID, _ := h.bootstrapAdmin(t)

	ctx := context.Background()
	plaintext, _, err := h.svc.CreateAPIKey(ctx, adminID, "ci key", []string{"write"}, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	rec := h.doWithHeaders(t, http.MethodGet, "/api/users", nil, map[string]string{
		"Authorization": "ApiKey " + plaintext,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Also confirm the X-API-Key header form works.
	rec2 := h.doWithHeaders(t, http.MethodGet, "/api/users", nil, map[string]string{
		"X-API-Key": plaintext,
	})
	if rec2.Code != http.StatusOK {
		t.Fatalf("X-API-Key status = %d; want 200; body=%s", rec2.Code, rec2.Body.String())
	}
}

// TestUserHandlers_APIKeyAuth_MissingScope verifies an API key without the
// "write" scope is rejected (falls through to the generic 401, since the
// credential itself is valid but does not satisfy the scope requirement).
func TestUserHandlers_APIKeyAuth_MissingScope(t *testing.T) {
	h := newUserHarness(t)
	adminID, _ := h.bootstrapAdmin(t)

	ctx := context.Background()
	plaintext, _, err := h.svc.CreateAPIKey(ctx, adminID, "read-only key", []string{"read"}, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	rec := h.doWithHeaders(t, http.MethodGet, "/api/users", nil, map[string]string{
		"X-API-Key": plaintext,
	})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401; body=%s", rec.Code, rec.Body.String())
	}
}

// TestUserHandlers_NoCredentials verifies the group rejects unauthenticated
// requests outright.
func TestUserHandlers_NoCredentials(t *testing.T) {
	h := newUserHarness(t)
	rec := h.doWithHeaders(t, http.MethodGet, "/api/users", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401; body=%s", rec.Code, rec.Body.String())
	}
}
