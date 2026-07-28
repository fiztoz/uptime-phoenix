// Package handlers_test contains integration tests for HTTP handlers.
package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/middleware"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// --- In-memory proxy repo for tests ---------------------------------------
//
// Mirrors fakeProxyRepo in internal/core/services/proxy_service_test.go, but
// lives in this package (handlers_test) so the handler tests don't have to
// reach into the services package's test-only type.

type fakeProxyRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.Proxy
	nextID int64
}

func newFakeProxyRepo() *fakeProxyRepo {
	return &fakeProxyRepo{byID: make(map[int64]*domain.Proxy)}
}

func (r *fakeProxyRepo) Create(_ context.Context, p *domain.Proxy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	p.ID = r.nextID
	cp := *p
	r.byID[p.ID] = &cp
	return nil
}

func (r *fakeProxyRepo) GetByID(_ context.Context, id int64) (*domain.Proxy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (r *fakeProxyRepo) List(_ context.Context, userID int64) ([]*domain.Proxy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Proxy, 0)
	for _, p := range r.byID {
		if userID > 0 && p.UserID != userID {
			continue
		}
		cp := *p
		out = append(out, &cp)
	}
	return out, nil
}

func (r *fakeProxyRepo) Update(_ context.Context, p *domain.Proxy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[p.ID]; !ok {
		return ports.ErrNotFound
	}
	cp := *p
	r.byID[p.ID] = &cp
	return nil
}

func (r *fakeProxyRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[id]; !ok {
		return ports.ErrNotFound
	}
	delete(r.byID, id)
	return nil
}

// getByIDForTest is a test-only escape hatch to inspect repo state directly
// (bypassing the HTTP layer) so ownership tests can assert a side effect
// (or the absence of one) instead of only checking the response status.
func (r *fakeProxyRepo) getByIDForTest(id int64) *domain.Proxy {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[id]
	if !ok {
		return nil
	}
	cp := *p
	return &cp
}

var _ ports.ProxyRepository = (*fakeProxyRepo)(nil)

// --- Test harness -----------------------------------------------------------
//
// Note: unlike maintenance/monitor, ProxyHandlers exposes no GET /:id
// endpoint (see internal/adapters/http/router.go — only POST/GET-list/PUT/
// DELETE are wired, matching web/src/lib/api/proxies.ts which never calls a
// single-proxy fetch; the frontend always works off the list). This is an
// intentional, existing design choice, not something introduced by this
// test — so "Get" coverage below exercises List instead of a nonexistent
// single-item endpoint.

type proxyTestHarness struct {
	router *echo.Echo
	repo   *fakeProxyRepo
	tokenA string // user A's JWT
	tokenB string // user B's JWT
}

func newProxyHarness(t *testing.T) *proxyTestHarness {
	t.Helper()

	userRepo := memory.NewUserRepo()
	apiKeyRepo := memory.NewAPIKeyRepo()
	authenticator := auth.NewJWTAuthenticator("test-key", 24, userRepo)
	totp := auth.NewTOTPProvider("Phoenix")
	authSvc := services.NewAuthService(userRepo, apiKeyRepo, authenticator, totp)

	ctx := context.Background()

	// User A: bootstrap user via Register (first user).
	if _, err := authSvc.Register(ctx, "proxy-user-a", "password123"); err != nil {
		t.Fatalf("register user A: %v", err)
	}
	tokenA, err := authSvc.Login(ctx, "proxy-user-a", "password123")
	if err != nil {
		t.Fatalf("login user A: %v", err)
	}

	// User B: created via the admin-supplied path (self-registration only
	// ever bootstraps the first user — see AGENTS.md Auth & User Management).
	if _, err := authSvc.CreateUser(ctx, "proxy-user-b", "password123", true, false, "UTC", services.UserCapabilities{}); err != nil {
		t.Fatalf("create user B: %v", err)
	}
	tokenB, err := authSvc.Login(ctx, "proxy-user-b", "password123")
	if err != nil {
		t.Fatalf("login user B: %v", err)
	}

	repo := newFakeProxyRepo()
	proxySvc := services.NewProxyService(repo)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	proxyH := handlers.NewProxyHandlers(proxySvc)
	proxyGroup := e.Group("/api/proxies", middleware.AuthMiddleware(authSvc))
	proxyGroup.POST("", proxyH.Create)
	proxyGroup.GET("", proxyH.List)
	proxyGroup.PUT("/:id", proxyH.Update)
	proxyGroup.DELETE("/:id", proxyH.Delete)

	return &proxyTestHarness{router: e, repo: repo, tokenA: tokenA, tokenB: tokenB}
}

func (h *proxyTestHarness) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
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

// --- CRUD tests ---------------------------------------------------------

// TestProxyHandlers_Create asserts POST /api/proxies returns 201 with the
// created proxy's non-secret fields echoed back.
func TestProxyHandlers_Create(t *testing.T) {
	h := newProxyHarness(t)

	body := map[string]any{
		"protocol": "http",
		"host":     "proxy.example.com",
		"port":     8080,
		"auth":     true,
		"username": "proxyuser",
		"password": "irrelevant-for-this-test",
	}
	rec := h.do(t, http.MethodPost, "/api/proxies", h.tokenA, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/proxies returned %d; want 201 (body: %s)", rec.Code, rec.Body.String())
	}

	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if created["host"] != "proxy.example.com" {
		t.Errorf("host = %v; want proxy.example.com", created["host"])
	}
	if created["protocol"] != "http" {
		t.Errorf("protocol = %v; want http", created["protocol"])
	}
	if created["username"] != "proxyuser" {
		t.Errorf("username = %v; want proxyuser (username IS safe to return)", created["username"])
	}
	id, ok := created["id"].(float64)
	if !ok || id <= 0 {
		t.Fatalf("id = %v; want a positive number", created["id"])
	}
	userID, ok := created["user_id"].(float64)
	if !ok || userID <= 0 {
		t.Fatalf("user_id = %v; want a positive number set from the authenticated context", created["user_id"])
	}
}

// TestProxyHandlers_Create_Unauthenticated asserts Create requires an
// authenticated principal (no bearer token = 401).
func TestProxyHandlers_Create_Unauthenticated(t *testing.T) {
	h := newProxyHarness(t)
	body := map[string]any{"protocol": "http", "host": "proxy.example.com", "port": 8080}
	rec := h.do(t, http.MethodPost, "/api/proxies", "", body)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/proxies (no token) returned %d; want 401", rec.Code)
	}
}

// TestProxyHandlers_List_ScopedToAuthenticatedUser asserts List only returns
// proxies owned by the caller — this is Phoenix's substitute for a
// single-item "Get" (see harness doc comment above: there is no GET /:id).
func TestProxyHandlers_List_ScopedToAuthenticatedUser(t *testing.T) {
	h := newProxyHarness(t)

	for _, host := range []string{"a1.example.com", "a2.example.com"} {
		body := map[string]any{"protocol": "http", "host": host, "port": 8080}
		rec := h.do(t, http.MethodPost, "/api/proxies", h.tokenA, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d (%s)", host, rec.Code, rec.Body.String())
		}
	}
	bRec := h.do(t, http.MethodPost, "/api/proxies", h.tokenB, map[string]any{
		"protocol": "http", "host": "b1.example.com", "port": 8080,
	})
	if bRec.Code != http.StatusCreated {
		t.Fatalf("create B-1: %d (%s)", bRec.Code, bRec.Body.String())
	}

	listA := h.do(t, http.MethodGet, "/api/proxies", h.tokenA, nil)
	if listA.Code != http.StatusOK {
		t.Fatalf("GET /api/proxies (user A) returned %d", listA.Code)
	}
	var proxiesA []map[string]any
	if err := json.Unmarshal(listA.Body.Bytes(), &proxiesA); err != nil {
		t.Fatalf("unmarshal list A: %v", err)
	}
	if len(proxiesA) != 2 {
		t.Fatalf("user A sees %d proxies; want 2 (list scoping is broken)", len(proxiesA))
	}
	for _, p := range proxiesA {
		if p["host"] == "b1.example.com" {
			t.Error("user A's list leaked user B's proxy")
		}
	}

	listB := h.do(t, http.MethodGet, "/api/proxies", h.tokenB, nil)
	if listB.Code != http.StatusOK {
		t.Fatalf("GET /api/proxies (user B) returned %d", listB.Code)
	}
	var proxiesB []map[string]any
	if err := json.Unmarshal(listB.Body.Bytes(), &proxiesB); err != nil {
		t.Fatalf("unmarshal list B: %v", err)
	}
	if len(proxiesB) != 1 {
		t.Errorf("user B sees %d proxies; want 1", len(proxiesB))
	}
}

// TestProxyHandlers_Update persists changes and returns them in the 200 body.
func TestProxyHandlers_Update(t *testing.T) {
	h := newProxyHarness(t)

	createRec := h.do(t, http.MethodPost, "/api/proxies", h.tokenA, map[string]any{
		"protocol": "http", "host": "old.example.com", "port": 8080,
	})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", createRec.Code, createRec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	id := floatToIntStr(created["id"].(float64))

	updateRec := h.do(t, http.MethodPut, "/api/proxies/"+id, h.tokenA, map[string]any{
		"protocol": "socks5", "host": "new.example.com", "port": 1080, "is_default": true,
	})
	if updateRec.Code != http.StatusOK {
		t.Fatalf("PUT /api/proxies/%s returned %d; want 200 (body: %s)", id, updateRec.Code, updateRec.Body.String())
	}
	var updated map[string]any
	if err := json.Unmarshal(updateRec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if updated["host"] != "new.example.com" {
		t.Errorf("host = %v; want new.example.com", updated["host"])
	}
	if updated["protocol"] != "socks5" {
		t.Errorf("protocol = %v; want socks5", updated["protocol"])
	}
	if updated["is_default"] != true {
		t.Errorf("is_default = %v; want true", updated["is_default"])
	}

	// Confirm the change is actually persisted, not just echoed.
	listRec := h.do(t, http.MethodGet, "/api/proxies", h.tokenA, nil)
	var list []map[string]any
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0]["host"] != "new.example.com" {
		t.Fatalf("list after update = %v; want a single proxy with host new.example.com", list)
	}
}

// TestProxyHandlers_Delete removes the proxy so it no longer appears in List.
func TestProxyHandlers_Delete(t *testing.T) {
	h := newProxyHarness(t)

	createRec := h.do(t, http.MethodPost, "/api/proxies", h.tokenA, map[string]any{
		"protocol": "http", "host": "todelete.example.com", "port": 8080,
	})
	var created map[string]any
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	id := floatToIntStr(created["id"].(float64))

	deleteRec := h.do(t, http.MethodDelete, "/api/proxies/"+id, h.tokenA, nil)
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /api/proxies/%s returned %d; want 204 (body: %s)", id, deleteRec.Code, deleteRec.Body.String())
	}

	listRec := h.do(t, http.MethodGet, "/api/proxies", h.tokenA, nil)
	var list []map[string]any
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("list after delete = %v; want empty", list)
	}
}

// --- Password-leak guard --------------------------------------------------

// TestProxyHandlers_PasswordNeverLeaksInResponseBody is a CRITICAL regression
// guard: domain.Proxy.Password is plaintext (it has to be dial-able at check
// time) and ProxyView intentionally omits it. This test creates a proxy with
// a distinctive password and asserts that exact string never appears in the
// raw response body of Create, List, or Update — across both the initial
// password and a password rotated in via Update. If anyone ever adds a
// Password field to ProxyView (or forgets to strip it in a future handler
// change), this test fails loudly instead of silently shipping a credential
// leak.
//
// There is no GET /api/proxies/:id endpoint to check (see harness doc
// comment) — List is the only read path and is covered below.
func TestProxyHandlers_PasswordNeverLeaksInResponseBody(t *testing.T) {
	h := newProxyHarness(t)
	const secret = "s3cr3t-do-not-leak"

	createRec := h.do(t, http.MethodPost, "/api/proxies", h.tokenA, map[string]any{
		"protocol": "http",
		"host":     "leaktest.example.com",
		"port":     8080,
		"auth":     true,
		"username": "proxyuser",
		"password": secret,
	})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", createRec.Code, createRec.Body.String())
	}
	if strings.Contains(createRec.Body.String(), secret) {
		t.Fatalf("CREATE response body leaks the plaintext password: %s", createRec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	id := floatToIntStr(created["id"].(float64))

	listRec := h.do(t, http.MethodGet, "/api/proxies", h.tokenA, nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list: %d (%s)", listRec.Code, listRec.Body.String())
	}
	if strings.Contains(listRec.Body.String(), secret) {
		t.Fatalf("LIST response body leaks the plaintext password: %s", listRec.Body.String())
	}

	const rotatedSecret = "r0tated-also-do-not-leak"
	updateRec := h.do(t, http.MethodPut, "/api/proxies/"+id, h.tokenA, map[string]any{
		"protocol": "http",
		"host":     "leaktest.example.com",
		"port":     8080,
		"auth":     true,
		"username": "proxyuser",
		"password": rotatedSecret,
	})
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update: %d (%s)", updateRec.Code, updateRec.Body.String())
	}
	if strings.Contains(updateRec.Body.String(), secret) {
		t.Fatalf("UPDATE response body leaks the ORIGINAL plaintext password: %s", updateRec.Body.String())
	}
	if strings.Contains(updateRec.Body.String(), rotatedSecret) {
		t.Fatalf("UPDATE response body leaks the ROTATED plaintext password: %s", updateRec.Body.String())
	}

	// One more List after rotation, for good measure — the rotated secret
	// must not leak on any subsequent read either.
	listAfterUpdate := h.do(t, http.MethodGet, "/api/proxies", h.tokenA, nil)
	if strings.Contains(listAfterUpdate.Body.String(), rotatedSecret) {
		t.Fatalf("post-rotation LIST response body leaks the plaintext password: %s", listAfterUpdate.Body.String())
	}

	// Sanity check the guard itself isn't vacuous: confirm the password was
	// actually stored (in the repo, out-of-band of the HTTP layer) so we
	// know the assertions above are testing a real credential, not one that
	// silently failed to save.
	idInt, err := parseIDForTest(id)
	if err != nil {
		t.Fatalf("parse id: %v", err)
	}
	stored := h.repo.getByIDForTest(idInt)
	if stored == nil {
		t.Fatal("proxy not found in repo after update")
	}
	if stored.Password != rotatedSecret {
		t.Fatalf("repo Password = %q; want %q (guard would be vacuous otherwise)", stored.Password, rotatedSecret)
	}
}

// --- Ownership enforcement -------------------------------------------------

// TestProxyHandlers_Update_EnforcesOwnership asserts a non-owner's Update is
// rejected with 404 AND — critically — that the record was not mutated. A
// previous bug elsewhere in this codebase returned the error body for an
// ownership failure but still performed the underlying mutation; this
// asserts the actual repo state, not just the HTTP status code.
func TestProxyHandlers_Update_EnforcesOwnership(t *testing.T) {
	h := newProxyHarness(t)

	createRec := h.do(t, http.MethodPost, "/api/proxies", h.tokenA, map[string]any{
		"protocol": "http", "host": "owned-by-a.example.com", "port": 8080, "username": "original-user",
	})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", createRec.Code, createRec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	id := floatToIntStr(created["id"].(float64))
	idInt, err := parseIDForTest(id)
	if err != nil {
		t.Fatalf("parse id: %v", err)
	}

	// User B attempts to overwrite user A's proxy.
	otherRec := h.do(t, http.MethodPut, "/api/proxies/"+id, h.tokenB, map[string]any{
		"protocol": "socks5", "host": "hijacked.example.com", "port": 1080, "username": "attacker",
	})
	if otherRec.Code != http.StatusNotFound {
		t.Errorf("non-owner PUT /api/proxies/%s returned %d; want 404", id, otherRec.Code)
	}

	// Side-effect assertion: the record itself must be untouched, checked
	// directly against the repo (bypassing the HTTP layer entirely) so a
	// handler bug that mutates-then-403s can't hide behind a correct status
	// code.
	stored := h.repo.getByIDForTest(idInt)
	if stored == nil {
		t.Fatal("proxy vanished from repo after a rejected non-owner update")
	}
	if stored.Host != "owned-by-a.example.com" || stored.Protocol != "http" || stored.Username != "original-user" {
		t.Fatalf("proxy was mutated by a rejected non-owner update: %+v", stored)
	}

	// Also verify via the owner's own read path (List) for good measure.
	listRec := h.do(t, http.MethodGet, "/api/proxies", h.tokenA, nil)
	var list []map[string]any
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0]["host"] != "owned-by-a.example.com" {
		t.Fatalf("owner's list after rejected hijack attempt = %v; want unchanged original", list)
	}
}

// TestProxyHandlers_Delete_EnforcesOwnership asserts a non-owner's Delete is
// rejected with 404 AND that the record still exists afterward (same
// side-effect-not-just-status-code discipline as the Update test above).
func TestProxyHandlers_Delete_EnforcesOwnership(t *testing.T) {
	h := newProxyHarness(t)

	createRec := h.do(t, http.MethodPost, "/api/proxies", h.tokenA, map[string]any{
		"protocol": "http", "host": "keep-me.example.com", "port": 8080,
	})
	var created map[string]any
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	id := floatToIntStr(created["id"].(float64))
	idInt, err := parseIDForTest(id)
	if err != nil {
		t.Fatalf("parse id: %v", err)
	}

	otherRec := h.do(t, http.MethodDelete, "/api/proxies/"+id, h.tokenB, nil)
	if otherRec.Code != http.StatusNotFound {
		t.Errorf("non-owner DELETE /api/proxies/%s returned %d; want 404", id, otherRec.Code)
	}

	// Side-effect assertion straight against the repo: the row must still
	// exist. This is the exact shape of bug the task description warns
	// about — a handler that writes the 404 error body but calls
	// svc.Delete anyway.
	if stored := h.repo.getByIDForTest(idInt); stored == nil {
		t.Fatal("proxy was deleted by a non-owner despite a 404 response")
	}

	// Owner can still see and then successfully delete it.
	getRec := h.do(t, http.MethodGet, "/api/proxies", h.tokenA, nil)
	var list []map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("owner's list after rejected non-owner delete = %v; want 1 surviving proxy", list)
	}

	ownRec := h.do(t, http.MethodDelete, "/api/proxies/"+id, h.tokenA, nil)
	if ownRec.Code != http.StatusNoContent {
		t.Errorf("owner DELETE /api/proxies/%s returned %d; want 204", id, ownRec.Code)
	}
}

// parseIDForTest converts the decimal ID string produced by floatToIntStr
// back into an int64 for direct repo lookups.
func parseIDForTest(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
