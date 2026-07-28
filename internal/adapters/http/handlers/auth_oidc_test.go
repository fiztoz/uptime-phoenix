package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type oidcFake struct {
	enabled bool
	claims  *ports.OIDCClaims
	exchErr error
	state   string
	nonce   string
}

func (f *oidcFake) Enabled() bool  { return f.enabled }
func (f *oidcFake) Issuer() string { return "https://idp.example.com" }
func (f *oidcFake) AuthCodeURL(state, nonce string) string {
	f.state, f.nonce = state, nonce
	return "https://idp.example.com/auth?state=" + state
}
func (f *oidcFake) Exchange(_ context.Context, _, _ string) (*ports.OIDCClaims, error) {
	if f.exchErr != nil {
		return nil, f.exchErr
	}
	return f.claims, nil
}
func (f *oidcFake) EndSessionURL(string) string { return "" }

type oidcAuthn struct{}

func (o *oidcAuthn) Login(context.Context, string, string) (string, error) { return "tok", nil }
func (o *oidcAuthn) VerifyToken(context.Context, string) (int64, error)    { return 1, nil }
func (o *oidcAuthn) HashPassword(pw string) (string, error)                { return "h:" + pw, nil }
func (o *oidcAuthn) VerifyPassword(h, pw string) error {
	if h != "h:"+pw {
		return context.Canceled
	}
	return nil
}
func (o *oidcAuthn) IssueSession(context.Context, int64) (string, error) { return "session-jwt", nil }
func (o *oidcAuthn) IssuePending2FATicket(context.Context, int64) (string, error) {
	return "t", nil
}
func (o *oidcAuthn) VerifyPending2FATicket(context.Context, string) (int64, error) {
	return 1, nil
}

type oidc2FA struct{}

func (o *oidc2FA) GenerateSecret(string, string) (string, string, error) {
	return "s", "u", nil
}
func (o *oidc2FA) VerifyToken(string, string) bool { return true }

type clk struct{ t time.Time }

func (c clk) Now() time.Time { return c.t }

func newOIDCHandler(t *testing.T, oidc *oidcFake, policy services.OIDCPolicy) *handlers.AuthHandlers {
	t.Helper()
	users := memory.NewUserRepo()
	apiKeys := memory.NewAPIKeyRepo()
	idents := memory.NewOIDCIdentityRepo()
	perms := memory.NewUserPermissionRepo()
	if policy.StateSecret == "" {
		policy.StateSecret = "handler-secret"
	}
	if policy.StateTTL == 0 {
		policy.StateTTL = time.Minute
	}
	policy.FrontendRedirect = ""
	svc := services.NewAuthService(
		users, apiKeys, &oidcAuthn{}, &oidc2FA{},
		services.WithClock(clk{t: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)}),
		services.WithOIDC(oidc, idents, perms, policy),
	)
	return handlers.NewAuthHandlers(svc)
}

func TestOIDCStatus_Disabled(t *testing.T) {
	// No WithOIDC → disabled.
	users := memory.NewUserRepo()
	svc := services.NewAuthService(users, memory.NewAPIKeyRepo(), &oidcAuthn{}, &oidc2FA{})
	h := handlers.NewAuthHandlers(svc)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/status", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.OIDCStatus(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var body handlers.OIDCStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Enabled {
		t.Fatal("expected disabled")
	}
}

func TestOIDCStatus_Enabled(t *testing.T) {
	h := newOIDCHandler(t, &oidcFake{enabled: true}, services.OIDCPolicy{JITEnabled: true})
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/status", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.OIDCStatus(c); err != nil {
		t.Fatal(err)
	}
	var body handlers.OIDCStatusResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !body.Enabled {
		t.Fatal("expected enabled")
	}
}

func TestOIDCLogin_Redirects(t *testing.T) {
	fake := &oidcFake{enabled: true}
	h := newOIDCHandler(t, fake, services.OIDCPolicy{JITEnabled: true})
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/login", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.OIDCLogin(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("code %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://idp.example.com/auth") {
		t.Fatalf("location %q", loc)
	}
}

func TestOIDCLogin_NotConfigured(t *testing.T) {
	users := memory.NewUserRepo()
	svc := services.NewAuthService(users, memory.NewAPIKeyRepo(), &oidcAuthn{}, &oidc2FA{})
	h := handlers.NewAuthHandlers(svc)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/login", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.OIDCLogin(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestOIDCCallback_SuccessRedirect(t *testing.T) {
	fake := &oidcFake{enabled: true}
	h := newOIDCHandler(t, fake, services.OIDCPolicy{JITEnabled: true})
	// Mint state via Begin path.
	e := echo.New()
	loginReq := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/login", nil)
	loginRec := httptest.NewRecorder()
	if err := h.OIDCLogin(e.NewContext(loginReq, loginRec)); err != nil {
		t.Fatal(err)
	}
	state := fake.state
	fake.claims = &ports.OIDCClaims{
		Issuer:  "https://idp.example.com",
		Subject: "cb-sub",
	}

	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?code=abc&state="+state, nil)
	rec := httptest.NewRecorder()
	if err := h.OIDCCallback(e.NewContext(req, rec)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "#oidc_token=session-jwt") {
		t.Fatalf("location %q", loc)
	}
}

func TestOIDCCallback_ErrorRedirect(t *testing.T) {
	fake := &oidcFake{enabled: true}
	h := newOIDCHandler(t, fake, services.OIDCPolicy{JITEnabled: true})
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?error=access_denied", nil)
	rec := httptest.NewRecorder()
	if err := h.OIDCCallback(e.NewContext(req, rec)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("code %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "oidc_error=access_denied") {
		t.Fatalf("location %q", rec.Header().Get("Location"))
	}
}
