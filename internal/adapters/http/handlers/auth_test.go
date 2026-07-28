package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/middleware"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// --- test wiring --------------------------------------------------------

// testHarness wires the minimum set of adapters needed to drive the
// auth HTTP handlers end-to-end. Each call returns a fresh Echo router
// pre-configured with the auth routes so tests get a clean slate and
// can run in parallel without interfering.
type testHarness struct {
	router *echo.Echo
	svc    *services.AuthService
	users  *memory.UserRepo
}

func newHarness(t *testing.T) *testHarness {
	t.Helper()

	userRepo := memory.NewUserRepo()
	apiKeyRepo := memory.NewAPIKeyRepo()
	webAuthnRepo := memory.NewWebAuthnCredentialRepo()
	// A fixed signing key keeps tokens stable across sub-tests, but it
	// is not used in assertions — the handlers do not verify JWTs.
	authenticator := auth.NewJWTAuthenticator("test-signing-key-do-not-use-in-prod", 24, userRepo)
	totp := auth.NewTOTPProvider("Phoenix")
	webAuthnProvider, err := auth.NewWebAuthnProvider(auth.WebAuthnConfig{
		RPID:      "localhost",
		RPOrigins: []string{"http://localhost:3000"},
	})
	if err != nil {
		t.Fatalf("NewWebAuthnProvider: %v", err)
	}
	svc := services.NewAuthService(userRepo, apiKeyRepo, authenticator, totp,
		services.WithWebAuthn(webAuthnProvider, webAuthnRepo))

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	// Mirror the production router's public/protected split.
	authH := handlers.NewAuthHandlers(svc)
	authGroup := e.Group("/api/auth")
	authGroup.POST("/register", authH.Register)
	authGroup.POST("/login", authH.Login)
	authGroup.POST("/verify-2fa", authH.Verify2FA)
	authGroup.POST("/webauthn/login/begin", authH.WebAuthnLoginBegin)
	authGroup.POST("/webauthn/login/finish", authH.WebAuthnLoginFinish)
	protectedGroup := e.Group("/api/auth", middleware.AuthMiddleware(svc))
	protectedGroup.GET("/me", authH.Me)
	protectedGroup.POST("/setup-2fa", authH.Setup2FA)
	protectedGroup.POST("/enable-2fa", authH.Enable2FA)
	protectedGroup.POST("/disable-2fa", authH.Disable2FA)
	protectedGroup.POST("/webauthn/register/begin", authH.WebAuthnRegisterBegin)
	protectedGroup.POST("/webauthn/register/finish", authH.WebAuthnRegisterFinish)
	protectedGroup.GET("/webauthn/credentials", authH.WebAuthnListCredentials)
	protectedGroup.DELETE("/webauthn/credentials/:id", authH.WebAuthnDeleteCredential)

	// API key routes (mirror production router).
	apiKeyH := handlers.NewAPIKeyHandlers(svc)
	keyGroup := e.Group("/api/api-keys", middleware.AuthMiddleware(svc))
	keyGroup.POST("", apiKeyH.Create)
	keyGroup.GET("", apiKeyH.List)
	keyGroup.DELETE("/:id", apiKeyH.Delete)

	return &testHarness{router: e, svc: svc, users: userRepo}
}

func (h *testHarness) do(t *testing.T, method, path string, body any, token string) *httptest.ResponseRecorder {
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
	if token != "" {
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

// --- tests --------------------------------------------------------------

func TestAuthHandlers_Register_Success(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "alice",
		"password": "supersecret",
	}, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s); want 201; body=%s", rec.Code, http.StatusText(rec.Code), rec.Body.String())
	}
	var resp handlers.LoginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Token == "" {
		t.Errorf("Token is empty")
	}
	if resp.User == nil {
		t.Fatalf("User is nil")
	}
	if resp.User.Username != "alice" {
		t.Errorf("Username = %q; want alice", resp.User.Username)
	}
}

func TestAuthHandlers_Register_ShortPassword(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "alice",
		"password": "short",
	}, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

// TestAuthHandlers_Register_DisabledAfterFirstUser verifies self-registration
// closes as soon as the install has its first user: a second call to
// /api/auth/register — regardless of whether the username collides — must
// be rejected with 403, not delegate down to the username-conflict check.
// Creating additional users after the first is only possible via the
// admin-only POST /api/users (see user_test.go).
func TestAuthHandlers_Register_DisabledAfterFirstUser(t *testing.T) {
	h := newHarness(t)
	body := map[string]string{"username": "alice", "password": "supersecret"}
	first := h.do(t, http.MethodPost, "/api/auth/register", body, "")
	if first.Code != http.StatusCreated {
		t.Fatalf("first register status = %d; want 201", first.Code)
	}
	rec := h.do(t, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "bob", "password": "supersecret",
	}, "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403 Forbidden", rec.Code)
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["error"] != "registration is disabled" {
		t.Errorf("error = %q; want %q", resp["error"], "registration is disabled")
	}
}

func TestAuthHandlers_Login_InvalidCredentials(t *testing.T) {
	h := newHarness(t)
	_ = h.do(t, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "alice",
		"password": "supersecret",
	}, "")
	rec := h.do(t, http.MethodPost, "/api/auth/login", map[string]string{
		"username": "alice",
		"password": "wrong",
	}, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", rec.Code)
	}
}

func TestAuthHandlers_Login_Success(t *testing.T) {
	h := newHarness(t)
	_ = h.do(t, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "alice",
		"password": "supersecret",
	}, "")
	rec := h.do(t, http.MethodPost, "/api/auth/login", map[string]string{
		"username": "alice",
		"password": "supersecret",
	}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp handlers.LoginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Token == "" {
		t.Errorf("Token is empty")
	}
	if resp.Requires2FA {
		t.Errorf("Requires2FA should be false for a user without TOTP")
	}
}

// TestAuthHandlers_Login_Requires2FA walks the full split-login flow:
//  1. register
//  2. setup + enable 2FA (via the service directly — the handler path
//     requires a real TOTP code, which we cannot mint synchronously
//     from a unit test without a TOTP library)
//  3. login → expect {requires_2fa: true, ticket}
//  4. verify-2fa with a stale code → expect 401
func TestAuthHandlers_Login_Requires2FA(t *testing.T) {
	h := newHarness(t)
	register := h.do(t, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "alice",
		"password": "supersecret",
	}, "")
	if register.Code != http.StatusCreated {
		t.Fatalf("register status = %d; want 201; body=%s", register.Code, register.Body.String())
	}
	// Register auto-logs in; we need that token to call /setup-2fa.
	var regResp handlers.LoginResponse
	if err := json.Unmarshal(register.Body.Bytes(), &regResp); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	if regResp.Token == "" {
		t.Fatal("register did not return a token")
	}

	// Setup 2FA (does not require a valid TOTP code, just generates).
	setup := h.do(t, http.MethodPost, "/api/auth/setup-2fa", nil, regResp.Token)
	if setup.Code != http.StatusOK {
		t.Fatalf("setup-2fa status = %d; want 200; body=%s", setup.Code, setup.Body.String())
	}

	// Enable 2FA by reaching into the service — the handler path needs
	// a real 6-digit code, which we cannot mint without standing up a
	// TOTP library inside the test. This is an acceptable trade-off
	// for a unit test: the handler tests for /enable-2fa and
	// /verify-2fa exercise the HTTP layer, and the service tests
	// cover the validation logic.
	if err := h.svc.EnableTOTP(context.Background(), 1, "000000"); err == nil {
		t.Fatalf("EnableTOTP with stale code should fail")
	}
	// Forcibly enable 2FA by toggling the user record.
	user, err := h.users.GetByID(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	user.TOTPEnabled = true
	if err := h.users.Update(context.Background(), user); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Now login should require 2FA.
	login := h.do(t, http.MethodPost, "/api/auth/login", map[string]string{
		"username": "alice",
		"password": "supersecret",
	}, "")
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d; want 200; body=%s", login.Code, login.Body.String())
	}
	var loginResp handlers.LoginResponse
	if err := json.Unmarshal(login.Body.Bytes(), &loginResp); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if !loginResp.Requires2FA {
		t.Errorf("Requires2FA = false; want true for a user with 2FA enabled")
	}
	if loginResp.Ticket == "" {
		t.Errorf("Ticket is empty; the handler should have issued one")
	}
	if loginResp.Token != "" {
		t.Errorf("Token should be empty on the 2FA challenge response")
	}

	// Submit a bad TOTP code via verify-2fa.
	bad := h.do(t, http.MethodPost, "/api/auth/verify-2fa", map[string]string{
		"ticket": loginResp.Ticket,
		"token":  "000000",
	}, "")
	if bad.Code != http.StatusUnauthorized {
		t.Errorf("verify-2fa (bad code) status = %d; want 401; body=%s", bad.Code, bad.Body.String())
	}
}

func TestAuthHandlers_Me_Unauthorized(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, http.MethodGet, "/api/auth/me", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", rec.Code)
	}
}

func TestAuthHandlers_Me_Authorized(t *testing.T) {
	h := newHarness(t)
	reg := h.do(t, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "alice",
		"password": "supersecret",
	}, "")
	var resp handlers.LoginResponse
	if err := json.Unmarshal(reg.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rec := h.do(t, http.MethodGet, "/api/auth/me", nil, resp.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"username":"alice"`) {
		t.Errorf("body does not include username: %s", rec.Body.String())
	}
}

func TestAuthHandlers_Register_BadJSON(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader("{not json"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

// --- WebAuthn (passkey) handler tests -----------------------------------

// registerAndLogin registers a user and returns its session token.
func (h *testHarness) registerAndLogin(t *testing.T) string {
	t.Helper()
	rec := h.do(t, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "alice",
		"password": "supersecret",
	}, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp handlers.LoginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("register returned no token")
	}
	return resp.Token
}

func TestWebAuthn_RegisterBegin_RequiresAuth(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, http.MethodPost, "/api/auth/webauthn/register/begin", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", rec.Code)
	}
}

func TestWebAuthn_RegisterBegin_ReturnsOptions(t *testing.T) {
	h := newHarness(t)
	token := h.registerAndLogin(t)

	rec := h.do(t, http.MethodPost, "/api/auth/webauthn/register/begin", nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp handlers.WebAuthnBeginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode begin: %v", err)
	}
	if resp.SessionID == "" {
		t.Error("missing session_id")
	}
	if len(resp.PublicKey) == 0 || !strings.Contains(string(resp.PublicKey), "challenge") {
		t.Errorf("publicKey options missing challenge: %s", resp.PublicKey)
	}
	var publicKey map[string]any
	if err := json.Unmarshal(resp.PublicKey, &publicKey); err != nil {
		t.Fatalf("decode publicKey options: %v", err)
	}
	if _, ok := publicKey["challenge"]; !ok {
		t.Errorf("publicKey challenge is not at the browser-facing top level: %s", resp.PublicKey)
	}
	if _, nested := publicKey["publicKey"]; nested {
		t.Errorf("publicKey options are double-wrapped: %s", resp.PublicKey)
	}
}

func TestWebAuthn_ListCredentials_Empty(t *testing.T) {
	h := newHarness(t)
	token := h.registerAndLogin(t)

	rec := h.do(t, http.MethodGet, "/api/auth/webauthn/credentials", nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("body = %s; want []", rec.Body.String())
	}
}

func TestWebAuthn_DeleteCredential_BadID(t *testing.T) {
	h := newHarness(t)
	token := h.registerAndLogin(t)

	rec := h.do(t, http.MethodDelete, "/api/auth/webauthn/credentials/not-a-number", nil, token)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestWebAuthn_LoginBegin_NoSuchUser(t *testing.T) {
	h := newHarness(t)
	// Unknown username is mapped to invalid credentials (401) to avoid
	// leaking which usernames exist.
	rec := h.do(t, http.MethodPost, "/api/auth/webauthn/login/begin", map[string]string{"username": "ghost"}, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestWebAuthn_LoginBegin_NoCredentials(t *testing.T) {
	h := newHarness(t)
	h.registerAndLogin(t)
	// alice exists but has no passkeys → 404 no_credentials.
	rec := h.do(t, http.MethodPost, "/api/auth/webauthn/login/begin", map[string]string{"username": "alice"}, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestWebAuthn_LoginFinish_MissingFields(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, http.MethodPost, "/api/auth/webauthn/login/finish", map[string]string{"username": "alice"}, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

// Compile-time guard: confirm the adapter in the wiring satisfies the
// port so any future refactor of the auth package fails this build
// rather than the next test run.
var _ ports.UserRepository = (*memory.UserRepo)(nil)
var _ ports.Clock = (interface{ Now() time.Time })(nil)
var _ ports.TwoFactor = (*auth.TOTPProvider)(nil)
var _ ports.Authenticator = (*auth.JWTAuthenticator)(nil)
var _ ports.WebAuthnAuthenticator = (*auth.WebAuthnProvider)(nil)
var _ ports.WebAuthnCredentialRepository = (*memory.WebAuthnCredentialRepo)(nil)
