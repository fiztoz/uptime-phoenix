package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// --- fakes -----------------------------------------------------------------

type escHFakePolicyRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.EscalationPolicy
	nextID int64
}

func newEscHFakePolicyRepo() *escHFakePolicyRepo {
	return &escHFakePolicyRepo{byID: map[int64]*domain.EscalationPolicy{}, nextID: 1}
}

func (r *escHFakePolicyRepo) clone(p *domain.EscalationPolicy) *domain.EscalationPolicy {
	cp := *p
	cp.Steps = append([]domain.EscalationStep(nil), p.Steps...)
	return &cp
}

func (r *escHFakePolicyRepo) Create(_ context.Context, p *domain.EscalationPolicy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p.ID = r.nextID
	r.nextID++
	r.byID[p.ID] = r.clone(p)
	return nil
}

func (r *escHFakePolicyRepo) Update(_ context.Context, p *domain.EscalationPolicy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[p.ID]; !ok {
		return ports.ErrNotFound
	}
	r.byID[p.ID] = r.clone(p)
	return nil
}

func (r *escHFakePolicyRepo) GetByID(_ context.Context, id int64) (*domain.EscalationPolicy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return r.clone(p), nil
}

func (r *escHFakePolicyRepo) List(_ context.Context) ([]*domain.EscalationPolicy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.EscalationPolicy, 0, len(r.byID))
	for _, p := range r.byID {
		out = append(out, r.clone(p))
	}
	return out, nil
}

func (r *escHFakePolicyRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}

type escHFakeAssignRepo struct {
	mu       sync.Mutex
	monitors map[int64]int64
	groups   map[int64]int64
}

func newEscHFakeAssignRepo() *escHFakeAssignRepo {
	return &escHFakeAssignRepo{monitors: map[int64]int64{}, groups: map[int64]int64{}}
}

func (r *escHFakeAssignRepo) AssignMonitor(_ context.Context, monitorID, policyID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.monitors[monitorID] = policyID
	return nil
}

func (r *escHFakeAssignRepo) UnassignMonitor(_ context.Context, monitorID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.monitors, monitorID)
	return nil
}

func (r *escHFakeAssignRepo) PolicyIDForMonitor(_ context.Context, monitorID int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.monitors[monitorID]
	if !ok {
		return 0, ports.ErrNotFound
	}
	return id, nil
}

func (r *escHFakeAssignRepo) AssignGroup(_ context.Context, groupID, policyID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.groups[groupID] = policyID
	return nil
}

func (r *escHFakeAssignRepo) UnassignGroup(_ context.Context, groupID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.groups, groupID)
	return nil
}

func (r *escHFakeAssignRepo) PolicyIDForGroup(_ context.Context, groupID int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.groups[groupID]
	if !ok {
		return 0, ports.ErrNotFound
	}
	return id, nil
}

// escHFakeStateRepo is unused by the handler surface but required by the
// service constructor. Every method fails loudly rather than quietly
// succeeding: if a handler ever reaches the escalation state store, this suite
// should break rather than pass on a silent no-op (AGENTS.md rule 7).
type escHFakeStateRepo struct {
	ports.AlertEscalationRepository
}

// --- harness ---------------------------------------------------------------

type escHarnessHTTP struct {
	router *echo.Echo

	adminToken  string
	notifToken  string // non-admin WITH can_manage_notifications
	memberToken string // non-admin with neither capability

	policies *escHFakePolicyRepo
	assign   *escHFakeAssignRepo
}

func newEscHTTPHarness(t *testing.T) *escHarnessHTTP {
	t.Helper()
	ctx := context.Background()

	userRepo := memory.NewUserRepo()
	apiKeyRepo := memory.NewAPIKeyRepo()
	permRepo := memory.NewUserPermissionRepo()
	authenticator := auth.NewJWTAuthenticator("test-key", 24, userRepo)
	authSvc := services.NewAuthService(userRepo, apiKeyRepo, authenticator, auth.NewTOTPProvider("Phoenix"))

	if _, err := authSvc.Register(ctx, "esc-admin", "password123"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	adminToken, err := authSvc.Login(ctx, "esc-admin", "password123")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	if _, createErr := authSvc.CreateUser(ctx, "esc-notif", "password123", true, false, "UTC", services.UserCapabilities{
		CanManageNotifications: true,
	}); createErr != nil {
		t.Fatalf("create notification manager: %v", createErr)
	}
	notifToken, err := authSvc.Login(ctx, "esc-notif", "password123")
	if err != nil {
		t.Fatalf("login notification manager: %v", err)
	}
	if _, createErr := authSvc.CreateUser(ctx, "esc-member", "password123", true, false, "UTC", services.UserCapabilities{}); createErr != nil {
		t.Fatalf("create member: %v", createErr)
	}
	memberToken, err := authSvc.Login(ctx, "esc-member", "password123")
	if err != nil {
		t.Fatalf("login member: %v", err)
	}

	monitorRepo := newFakeMonitorRepo()
	groupRepo := newFakeMonitorGroupRepo()
	accessSvc := services.NewAccessService(userRepo, permRepo, groupRepo, monitorRepo)

	h := &escHarnessHTTP{
		adminToken:  adminToken,
		notifToken:  notifToken,
		memberToken: memberToken,
		policies:    newEscHFakePolicyRepo(),
		assign:      newEscHFakeAssignRepo(),
	}

	svc := services.NewEscalationService(
		h.policies, h.assign, &escHFakeStateRepo{},
		nil, monitorRepo, groupRepo, nil,
	)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	// This wiring MUST mirror internal/adapters/http/router.go. It is duplicated
	// rather than reused because router.go builds the whole app; a divergence
	// here is invisible and these tests would keep passing against gates
	// production does not have. If you change a gate there, change it here.
	requireAdmin := middleware.RequireAdmin(authSvc)
	requireNotifications := middleware.RequireCapability(accessSvc, middleware.CapManageNotifications)

	escH := handlers.NewEscalationHandlers(svc)
	eg := e.Group("/api/escalation-policies", middleware.AuthMiddleware(authSvc), requireNotifications)
	eg.GET("", escH.List)
	eg.POST("", escH.Create)
	eg.GET("/:id", escH.Get)
	eg.PUT("/:id", escH.Update)
	eg.DELETE("/:id", escH.Delete)

	mg := e.Group("/api/monitors/:id/escalation-policy",
		middleware.AuthMiddleware(authSvc), requireAdmin, requireNotifications)
	mg.GET("", escH.GetMonitorAssignment)
	mg.PUT("", escH.SetMonitorAssignment)

	gg := e.Group("/api/monitor-groups/:id/escalation-policy",
		middleware.AuthMiddleware(authSvc), requireAdmin, requireNotifications)
	gg.GET("", escH.GetGroupAssignment)
	gg.PUT("", escH.SetGroupAssignment)

	h.router = e
	return h
}

func (h *escHarnessHTTP) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
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

func (h *escHarnessHTTP) createPolicy(t *testing.T, token string, body any) map[string]any {
	t.Helper()
	rec := h.do(t, http.MethodPost, "/api/escalation-policies", token, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/escalation-policies = %d; want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// --- tests -----------------------------------------------------------------

func TestEscalationHandlers_CRUDRoundTrip(t *testing.T) {
	h := newEscHTTPHarness(t)

	created := h.createPolicy(t, h.adminToken, map[string]any{
		"name":        "Payments",
		"description": "on-call ladder",
		"steps": []map[string]any{
			{"wait_minutes": 5, "notification_ids": []int64{1}},
			{"wait_minutes": 10, "notification_ids": []int64{1, 2}},
		},
	})
	if created["enabled"] != true {
		t.Fatalf("enabled defaulted to %v; want true", created["enabled"])
	}
	steps, ok := created["steps"].([]any)
	if !ok || len(steps) != 2 {
		t.Fatalf("steps = %v; want 2", created["steps"])
	}
	// Step order is server-assigned from array position.
	first := steps[0].(map[string]any)
	if first["step_order"] != float64(1) {
		t.Fatalf("first step_order = %v; want 1", first["step_order"])
	}
	id := int64(created["id"].(float64))

	rec := h.do(t, http.MethodGet, "/api/escalation-policies", h.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET list = %d; want 200", rec.Code)
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list length = %d; want 1", len(list))
	}

	// Update replaces the ladder wholesale.
	rec = h.do(t, http.MethodPut, "/api/escalation-policies/1", h.adminToken, map[string]any{
		"name":    "Payments v2",
		"enabled": false,
		"steps":   []map[string]any{{"wait_minutes": 1, "notification_ids": []int64{9}}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d; want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var updated map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated["name"] != "Payments v2" || updated["enabled"] != false {
		t.Fatalf("update did not round-trip: %v", updated)
	}
	if len(updated["steps"].([]any)) != 1 {
		t.Fatalf("steps after replace-set = %v; want 1", updated["steps"])
	}

	rec = h.do(t, http.MethodDelete, "/api/escalation-policies/1", h.adminToken, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d; want 204", rec.Code)
	}
	rec = h.do(t, http.MethodGet, "/api/escalation-policies/1", h.adminToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET after delete = %d; want 404", rec.Code)
	}
	_ = id
}

func TestEscalationHandlers_UnknownPolicyIs404(t *testing.T) {
	h := newEscHTTPHarness(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/escalation-policies/999"},
		{http.MethodPut, "/api/escalation-policies/999"},
		{http.MethodDelete, "/api/escalation-policies/999"},
	} {
		rec := h.do(t, tc.method, tc.path, h.adminToken, map[string]any{
			"name": "x", "steps": []map[string]any{{"wait_minutes": 1, "notification_ids": []int64{1}}},
		})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d; want 404", tc.method, tc.path, rec.Code)
		}
	}
}

func TestEscalationHandlers_ValidationIs400(t *testing.T) {
	h := newEscHTTPHarness(t)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"blank name", map[string]any{"name": "  ", "steps": []map[string]any{{"wait_minutes": 1, "notification_ids": []int64{1}}}}},
		{"negative wait", map[string]any{"name": "p", "steps": []map[string]any{{"wait_minutes": -5, "notification_ids": []int64{1}}}}},
		{"step with no channels", map[string]any{"name": "p", "steps": []map[string]any{{"wait_minutes": 1, "notification_ids": []int64{}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(t, http.MethodPost, "/api/escalation-policies", h.adminToken, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST = %d; want 400 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// An admin has BOTH capability flags false yet may do everything — enforcement
// ORs them server-side (handoff §4.1). A gate that checked the raw flag alone
// would lock the admin out of their own escalation policies.
func TestEscalationHandlers_AdminWithRawFlagsFalseStillHasAccess(t *testing.T) {
	h := newEscHTTPHarness(t)
	rec := h.do(t, http.MethodGet, "/api/escalation-policies", h.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET list = %d; want 200 — the admin's raw can_manage_notifications is false", rec.Code)
	}
}

func TestEscalationHandlers_PolicyCRUDRequiresNotificationCapability(t *testing.T) {
	h := newEscHTTPHarness(t)
	h.createPolicy(t, h.adminToken, map[string]any{
		"name": "p", "steps": []map[string]any{{"wait_minutes": 1, "notification_ids": []int64{1}}},
	})

	// The notification manager holds the capability and may use the whole surface.
	if rec := h.do(t, http.MethodGet, "/api/escalation-policies", h.notifToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("notification manager GET list = %d; want 200", rec.Code)
	}
	if rec := h.do(t, http.MethodGet, "/api/escalation-policies/1", h.notifToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("notification manager GET one = %d; want 200", rec.Code)
	}

	// A plain member holds neither flag and is denied everywhere, reads included:
	// a step list names notification channels, which are already behind the
	// capability.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/escalation-policies"},
		{http.MethodGet, "/api/escalation-policies/1"},
		{http.MethodPost, "/api/escalation-policies"},
		{http.MethodPut, "/api/escalation-policies/1"},
		{http.MethodDelete, "/api/escalation-policies/1"},
	} {
		rec := h.do(t, tc.method, tc.path, h.memberToken, map[string]any{
			"name": "x", "steps": []map[string]any{{"wait_minutes": 1, "notification_ids": []int64{1}}},
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member %s %s = %d; want 403", tc.method, tc.path, rec.Code)
		}
	}
}

// Assignment rewires what a monitor does when it fails, so it is admin-only even
// for a holder of can_manage_notifications. Without this a non-admin
// notification manager could repoint the paging of a monitor they cannot see.
func TestEscalationHandlers_AssignmentIsAdminOnly(t *testing.T) {
	h := newEscHTTPHarness(t)
	h.createPolicy(t, h.adminToken, map[string]any{
		"name": "p", "steps": []map[string]any{{"wait_minutes": 1, "notification_ids": []int64{1}}},
	})

	for _, path := range []string{
		"/api/monitors/1/escalation-policy",
		"/api/monitor-groups/1/escalation-policy",
	} {
		for _, token := range []struct {
			name  string
			value string
		}{
			{"notification manager", h.notifToken},
			{"plain member", h.memberToken},
		} {
			rec := h.do(t, http.MethodPut, path, token.value, map[string]any{"policy_id": 1})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s PUT %s = %d; want 403", token.name, path, rec.Code)
			}
			rec = h.do(t, http.MethodGet, path, token.value, nil)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s GET %s = %d; want 403", token.name, path, rec.Code)
			}
		}
	}
}

func TestEscalationHandlers_AssignmentRoundTrip(t *testing.T) {
	h := newEscHTTPHarness(t)
	h.createPolicy(t, h.adminToken, map[string]any{
		"name": "p", "steps": []map[string]any{{"wait_minutes": 1, "notification_ids": []int64{1}}},
	})

	for _, path := range []string{
		"/api/monitors/1/escalation-policy",
		"/api/monitor-groups/1/escalation-policy",
	} {
		rec := h.do(t, http.MethodGet, path, h.adminToken, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d; want 200", path, rec.Code)
		}
		var view map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &view)
		if view["policy_id"] != float64(0) {
			t.Fatalf("unassigned policy_id = %v; want 0", view["policy_id"])
		}

		rec = h.do(t, http.MethodPut, path, h.adminToken, map[string]any{"policy_id": 1})
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d; want 200 (body: %s)", path, rec.Code, rec.Body.String())
		}

		// Assert the EFFECT, not the 200: read it back.
		rec = h.do(t, http.MethodGet, path, h.adminToken, nil)
		_ = json.Unmarshal(rec.Body.Bytes(), &view)
		if view["policy_id"] != float64(1) {
			t.Fatalf("after assign, policy_id = %v; want 1", view["policy_id"])
		}

		// policy_id 0 unassigns.
		rec = h.do(t, http.MethodPut, path, h.adminToken, map[string]any{"policy_id": 0})
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT unassign %s = %d; want 200", path, rec.Code)
		}
		rec = h.do(t, http.MethodGet, path, h.adminToken, nil)
		_ = json.Unmarshal(rec.Body.Bytes(), &view)
		if view["policy_id"] != float64(0) {
			t.Fatalf("after unassign, policy_id = %v; want 0", view["policy_id"])
		}
	}
}

func TestEscalationHandlers_AssignUnknownPolicyIs404(t *testing.T) {
	h := newEscHTTPHarness(t)
	rec := h.do(t, http.MethodPut, "/api/monitors/1/escalation-policy", h.adminToken, map[string]any{"policy_id": 4242})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("assign unknown policy = %d; want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestEscalationHandlers_UnauthenticatedIsRejected(t *testing.T) {
	h := newEscHTTPHarness(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/escalation-policies"},
		{http.MethodPost, "/api/escalation-policies"},
		{http.MethodPut, "/api/monitors/1/escalation-policy"},
	} {
		rec := h.do(t, tc.method, tc.path, "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s = %d; want 401", tc.method, tc.path, rec.Code)
		}
	}
}
