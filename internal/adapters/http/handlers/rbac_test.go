// Package handlers_test contains integration tests for HTTP handlers.
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
	"github.com/fiztoz/uptime-phoenix/internal/adapters/logger"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// --- Tag repo fakes (for the monitor wire-format tags) --------------------

type rbacFakeTagRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.Tag
	nextID int64
}

func newRBACFakeTagRepo() *rbacFakeTagRepo {
	return &rbacFakeTagRepo{byID: make(map[int64]*domain.Tag)}
}

func (r *rbacFakeTagRepo) Create(_ context.Context, t *domain.Tag) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	t.ID = r.nextID
	cp := *t
	r.byID[t.ID] = &cp
	return nil
}

func (r *rbacFakeTagRepo) GetByID(_ context.Context, id int64) (*domain.Tag, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

func (r *rbacFakeTagRepo) List(_ context.Context) ([]*domain.Tag, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Tag, 0, len(r.byID))
	for _, t := range r.byID {
		cp := *t
		out = append(out, &cp)
	}
	return out, nil
}

func (r *rbacFakeTagRepo) Update(_ context.Context, _ *domain.Tag) error { return nil }
func (r *rbacFakeTagRepo) Delete(_ context.Context, _ int64) error       { return nil }

type rbacFakeMonitorTagRepo struct {
	mu     sync.Mutex
	links  []domain.MonitorTag
	nextID int64
}

func newRBACFakeMonitorTagRepo() *rbacFakeMonitorTagRepo {
	return &rbacFakeMonitorTagRepo{}
}

func (r *rbacFakeMonitorTagRepo) Assign(_ context.Context, monitorID, tagID int64, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	r.links = append(r.links, domain.MonitorTag{ID: r.nextID, MonitorID: monitorID, TagID: tagID, Value: value})
	return nil
}

func (r *rbacFakeMonitorTagRepo) Remove(_ context.Context, _, _ int64) error { return nil }

func (r *rbacFakeMonitorTagRepo) ListByMonitor(_ context.Context, monitorID int64) ([]*domain.MonitorTag, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.MonitorTag, 0)
	for i := range r.links {
		if r.links[i].MonitorID == monitorID {
			cp := r.links[i]
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *rbacFakeMonitorTagRepo) ListByMonitors(ctx context.Context, monitorIDs []int64) (map[int64][]*domain.MonitorTag, error) {
	out := make(map[int64][]*domain.MonitorTag, len(monitorIDs))
	for _, id := range monitorIDs {
		links, err := r.ListByMonitor(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(links) > 0 {
			out[id] = links
		}
	}
	return out, nil
}

// --- Harness --------------------------------------------------------------

// rbacHarness wires the monitor + monitor-group endpoints behind the SAME
// middleware stack the production router uses, so the admin gate on mutations is
// under test too — not just the in-handler checks.
type rbacHarness struct {
	router *echo.Echo
	users  *memory.UserRepo

	adminToken  string // admin: sees and does everything
	memberToken string // non-admin, granted the visible folder, NO capabilities
	// creatorToken is a non-admin holding create_monitors + create_groups. Same
	// grants as the member — the pair exists so a test can tell "denied because
	// you have no capability" apart from "denied because it isn't yours".
	creatorToken string
	creatorID    int64

	monitorVisible int64
	monitorHidden  int64
	groupVisible   int64
	groupHidden    int64
	// groupCreator is granted ONLY to the creator. Creates land here so a
	// member with the shared visible-folder grant does not automatically see
	// every monitor the creator makes (group grants expand to contained monitors).
	groupCreator int64

	tagID int64
}

func newRBACHarness(t *testing.T) *rbacHarness {
	t.Helper()
	ctx := context.Background()

	userRepo := memory.NewUserRepo()
	apiKeyRepo := memory.NewAPIKeyRepo()
	permRepo := memory.NewUserPermissionRepo()
	authenticator := auth.NewJWTAuthenticator("test-key", 24, userRepo)
	totp := auth.NewTOTPProvider("Phoenix")
	authSvc := services.NewAuthService(userRepo, apiKeyRepo, authenticator, totp)

	if _, err := authSvc.Register(ctx, "rbac-admin", "password123"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	adminToken, err := authSvc.Login(ctx, "rbac-admin", "password123")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	member, err := authSvc.CreateUser(ctx, "rbac-member", "password123", true, false, "UTC", services.UserCapabilities{})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	memberToken, err := authSvc.Login(ctx, "rbac-member", "password123")
	if err != nil {
		t.Fatalf("login member: %v", err)
	}
	creator, err := authSvc.CreateUser(ctx, "rbac-creator", "password123", true, false, "UTC", services.UserCapabilities{
		CanCreateMonitors: true,
		CanCreateGroups:   true,
	})
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	creatorToken, err := authSvc.Login(ctx, "rbac-creator", "password123")
	if err != nil {
		t.Fatalf("login creator: %v", err)
	}

	monitorRepo := newFakeMonitorRepo()
	groupRepo := newFakeMonitorGroupRepo()
	bus := newFakeMonitorBus()
	monitorSvc := services.NewMonitorService(monitorRepo, bus)
	monitorSvc.SetGroupRepo(groupRepo)
	groupSvc := services.NewMonitorGroupService(groupRepo, monitorRepo, newFakeGroupHeartbeatRepo(), logger.New("error"))

	// Three folders: shared visible, hidden, and a creator-only create target.
	// The member is granted the shared folder (not individual monitors) so this
	// also exercises group-grant expansion end to end.
	visibleGroup := &domain.MonitorGroup{UserID: 1, Name: "visible", Condition: domain.GroupConditionWorstOfChildren}
	hiddenGroup := &domain.MonitorGroup{UserID: 1, Name: "hidden", Condition: domain.GroupConditionWorstOfChildren}
	creatorGroup := &domain.MonitorGroup{UserID: 1, Name: "creator-only", Condition: domain.GroupConditionWorstOfChildren}
	if err := groupRepo.Create(ctx, visibleGroup); err != nil {
		t.Fatalf("create visible group: %v", err)
	}
	if err := groupRepo.Create(ctx, hiddenGroup); err != nil {
		t.Fatalf("create hidden group: %v", err)
	}
	if err := groupRepo.Create(ctx, creatorGroup); err != nil {
		t.Fatalf("create creator group: %v", err)
	}

	visible := &domain.Monitor{UserID: 1, Name: "visible-monitor", Type: "http", Active: true, Interval: 60, GroupID: &visibleGroup.ID}
	hidden := &domain.Monitor{UserID: 1, Name: "hidden-monitor", Type: "http", Active: true, Interval: 60, GroupID: &hiddenGroup.ID}
	if err := monitorRepo.Create(ctx, visible); err != nil {
		t.Fatalf("create visible monitor: %v", err)
	}
	if err := monitorRepo.Create(ctx, hidden); err != nil {
		t.Fatalf("create hidden monitor: %v", err)
	}

	if err := permRepo.Grant(ctx, &domain.UserPermission{UserID: member.ID, GroupID: &visibleGroup.ID, IncludeDescendants: true}); err != nil {
		t.Fatalf("grant group: %v", err)
	}
	// Creator shares the member's view of the visible folder, PLUS a private
	// create target. Same sight on shared resources, different capabilities —
	// that isolates ownership — while private creates do not leak via the
	// member's group grant.
	if err := permRepo.Grant(ctx, &domain.UserPermission{UserID: creator.ID, GroupID: &visibleGroup.ID, IncludeDescendants: true}); err != nil {
		t.Fatalf("grant group to creator: %v", err)
	}
	if err := permRepo.Grant(ctx, &domain.UserPermission{UserID: creator.ID, GroupID: &creatorGroup.ID, IncludeDescendants: true}); err != nil {
		t.Fatalf("grant creator-only group: %v", err)
	}

	// A tag on the visible monitor, so the wire format has something to carry.
	tagRepo := newRBACFakeTagRepo()
	monitorTagRepo := newRBACFakeMonitorTagRepo()
	tag := &domain.Tag{Name: "prod", Color: "#ff0000"}
	if err := tagRepo.Create(ctx, tag); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := monitorTagRepo.Assign(ctx, visible.ID, tag.ID, "eu-west"); err != nil {
		t.Fatalf("assign tag: %v", err)
	}
	tagSvc := services.NewTagService(tagRepo, monitorTagRepo)

	accessSvc := services.NewAccessService(userRepo, permRepo, groupRepo, monitorRepo)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	// This wiring MUST mirror internal/adapters/http/router.go. It is duplicated
	// rather than reused because router.go builds the whole app, and a divergence
	// here is invisible: these tests would keep passing against gates production
	// does not have. If you change a gate in router.go, change it here too.
	//
	// In particular the mutation routes carry NO middleware, exactly as in
	// production — their gate is requireMonitorEditAccess inside the handler.
	// Re-adding requireAdmin here would make TestRBAC_Ownership_* pass for the
	// wrong reason and hide a real bypass.
	requireCreateMonitors := middleware.RequireCapability(accessSvc, middleware.CapCreateMonitors)
	requireCreateGroups := middleware.RequireCapability(accessSvc, middleware.CapCreateGroups)

	monitorH := handlers.NewMonitorHandlers(monitorSvc, accessSvc, tagSvc, groupRepo)
	mg := e.Group("/api/monitors", middleware.AuthMiddleware(authSvc))
	mg.GET("", monitorH.List)
	mg.GET("/:id", monitorH.GetByID)
	mg.POST("", monitorH.Create, requireCreateMonitors)
	mg.PUT("/:id", monitorH.Update)
	mg.DELETE("/:id", monitorH.Delete)
	mg.POST("/:id/clone", monitorH.Clone, requireCreateMonitors)

	groupH := handlers.NewMonitorGroupHandlers(groupSvc, accessSvc)
	gg := e.Group("/api/monitor-groups", middleware.AuthMiddleware(authSvc))
	gg.GET("", groupH.List)
	gg.GET("/:id", groupH.GetByID)
	gg.POST("", groupH.Create, requireCreateGroups)
	gg.PUT("/:id", groupH.Update)
	gg.DELETE("/:id", groupH.Delete)

	return &rbacHarness{
		router:         e,
		users:          userRepo,
		adminToken:     adminToken,
		memberToken:    memberToken,
		creatorToken:   creatorToken,
		creatorID:      creator.ID,
		monitorVisible: visible.ID,
		monitorHidden:  hidden.ID,
		groupVisible:   visibleGroup.ID,
		groupHidden:    hiddenGroup.ID,
		groupCreator:   creatorGroup.ID,
		tagID:          tag.ID,
	}
}

func (h *rbacHarness) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
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

// --- Tests: monitor visibility --------------------------------------------

// The core listing guarantee: a non-admin sees ONLY the monitors reachable from
// their grants. Asserting on the returned rows, not the status code — a 200
// carrying the whole install is exactly the bug this guards.
func TestRBAC_MonitorList_NonAdminSeesOnlyGrantedMonitors(t *testing.T) {
	h := newRBACHarness(t)

	rec := h.do(t, http.MethodGet, "/api/monitors", h.memberToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/monitors (member) = %d; want 200", rec.Code)
	}
	var monitors []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &monitors); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(monitors) != 1 {
		t.Fatalf("member sees %d monitors; want exactly 1 (the one reachable from their group grant)", len(monitors))
	}
	if int64(monitors[0]["id"].(float64)) != h.monitorVisible {
		t.Fatalf("member sees monitor %v; want %d", monitors[0]["id"], h.monitorVisible)
	}

	// The admin still sees both — the single-admin install is unchanged.
	adminRec := h.do(t, http.MethodGet, "/api/monitors", h.adminToken, nil)
	var adminMonitors []map[string]any
	if err := json.Unmarshal(adminRec.Body.Bytes(), &adminMonitors); err != nil {
		t.Fatalf("unmarshal admin list: %v", err)
	}
	if len(adminMonitors) != 2 {
		t.Fatalf("admin sees %d monitors; want 2", len(adminMonitors))
	}
}

// A monitor the caller cannot see must 404, not 403: a 403 confirms it exists.
func TestRBAC_MonitorGet_HiddenMonitorIs404(t *testing.T) {
	h := newRBACHarness(t)

	rec := h.do(t, http.MethodGet, "/api/monitors/"+intToStr(h.monitorHidden), h.memberToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET a monitor the member cannot see = %d; want 404 (403 would confirm it exists)", rec.Code)
	}

	ok := h.do(t, http.MethodGet, "/api/monitors/"+intToStr(h.monitorVisible), h.memberToken, nil)
	if ok.Code != http.StatusOK {
		t.Fatalf("GET the granted monitor = %d; want 200", ok.Code)
	}
}

// A non-admin with NO create capability is read-only on monitors, whatever they
// have been granted. Being able to SEE a monitor has never been permission to
// change it, and adding the create capabilities did not loosen that — this user
// simply holds none of them.
//
// The effect assertion is the re-fetch: a 403 that mutated anyway would be worse
// than no gate at all.
func TestRBAC_MonitorMutations_DeniedWithoutCapabilityOrOwnership(t *testing.T) {
	h := newRBACHarness(t)
	visible := intToStr(h.monitorVisible)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"create", http.MethodPost, "/api/monitors", map[string]any{
			"name": "member-made", "type": "http", "config": map[string]any{"url": "https://example.com"},
		}},
		{"update", http.MethodPut, "/api/monitors/" + visible, map[string]any{"name": "renamed-by-member"}},
		{"delete", http.MethodDelete, "/api/monitors/" + visible, nil},
		{"clone", http.MethodPost, "/api/monitors/" + visible + "/clone", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(t, tc.method, tc.path, h.memberToken, tc.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s as a non-admin = %d; want 403", tc.name, rec.Code)
			}
		})
	}

	// Nothing was written: the granted monitor still exists under its old name,
	// and the member's list still has exactly one monitor in it.
	get := h.do(t, http.MethodGet, "/api/monitors/"+visible, h.memberToken, nil)
	if get.Code != http.StatusOK {
		t.Fatalf("the granted monitor is gone after the rejected mutations: GET = %d", get.Code)
	}
	var monitor map[string]any
	if err := json.Unmarshal(get.Body.Bytes(), &monitor); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if monitor["name"] != "visible-monitor" {
		t.Errorf("monitor name = %v; a rejected PUT renamed it anyway", monitor["name"])
	}

	list := h.do(t, http.MethodGet, "/api/monitors", h.memberToken, nil)
	var monitors []map[string]any
	if err := json.Unmarshal(list.Body.Bytes(), &monitors); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(monitors) != 1 {
		t.Errorf("member sees %d monitors after the rejected create; want 1", len(monitors))
	}
}

// --- Tests: group visibility ----------------------------------------------

// The group tree a non-admin sees is the granted subtree — not the whole install.
func TestRBAC_GroupList_NonAdminSeesOnlyGrantedTree(t *testing.T) {
	h := newRBACHarness(t)

	rec := h.do(t, http.MethodGet, "/api/monitor-groups", h.memberToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/monitor-groups (member) = %d; want 200", rec.Code)
	}
	var groups []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("member sees %d groups; want exactly 1 (the granted one)", len(groups))
	}
	if int64(groups[0]["id"].(float64)) != h.groupVisible {
		t.Fatalf("member sees group %v; want %d", groups[0]["id"], h.groupVisible)
	}

	adminRec := h.do(t, http.MethodGet, "/api/monitor-groups", h.adminToken, nil)
	var adminGroups []map[string]any
	if err := json.Unmarshal(adminRec.Body.Bytes(), &adminGroups); err != nil {
		t.Fatalf("unmarshal admin groups: %v", err)
	}
	if len(adminGroups) != 3 {
		t.Fatalf("admin sees %d groups; want 3", len(adminGroups))
	}
}

// The group twin of TestRBAC_MonitorMutations_DeniedWithoutCapabilityOrOwnership:
// a non-admin holding no create capability cannot touch a folder, granted or not.
func TestRBAC_GroupMutations_DeniedWithoutCapabilityOrOwnership(t *testing.T) {
	h := newRBACHarness(t)
	visible := intToStr(h.groupVisible)

	if rec := h.do(t, http.MethodPost, "/api/monitor-groups", h.memberToken,
		map[string]any{"name": "member-folder"}); rec.Code != http.StatusForbidden {
		t.Errorf("POST /api/monitor-groups as a non-admin = %d; want 403", rec.Code)
	}
	if rec := h.do(t, http.MethodPut, "/api/monitor-groups/"+visible, h.memberToken,
		map[string]any{"name": "renamed"}); rec.Code != http.StatusForbidden {
		t.Errorf("PUT as a non-admin = %d; want 403", rec.Code)
	}
	if rec := h.do(t, http.MethodDelete, "/api/monitor-groups/"+visible, h.memberToken, nil); rec.Code != http.StatusForbidden {
		t.Errorf("DELETE as a non-admin = %d; want 403", rec.Code)
	}

	// Effect: the group survived and kept its name.
	get := h.do(t, http.MethodGet, "/api/monitor-groups/"+visible, h.memberToken, nil)
	if get.Code != http.StatusOK {
		t.Fatalf("granted group is gone after the rejected mutations: GET = %d", get.Code)
	}
	var group map[string]any
	if err := json.Unmarshal(get.Body.Bytes(), &group); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if group["name"] != "visible" {
		t.Errorf("group name = %v; a rejected PUT renamed it anyway", group["name"])
	}
}

// --- Tests: creation capabilities + ownership ------------------------------

// createMonitorAs POSTs a monitor and returns its id, failing the test if the
// create was refused.
func (h *rbacHarness) createMonitorAs(t *testing.T, token, name string) int64 {
	t.Helper()
	rec := h.do(t, http.MethodPost, "/api/monitors", token, map[string]any{
		"name": name, "type": "http", "group_id": h.groupCreator,
		"config": map[string]any{"url": "https://example.com"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/monitors = %d; want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created monitor: %v", err)
	}
	id, ok := created["id"].(float64)
	if !ok || id == 0 {
		t.Fatalf("created monitor has no usable id: %v", created)
	}
	return int64(id)
}

// monitorIDsVisibleTo returns the ids the caller's own list endpoint reports.
func (h *rbacHarness) monitorIDsVisibleTo(t *testing.T, token string) map[int64]bool {
	t.Helper()
	rec := h.do(t, http.MethodGet, "/api/monitors", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/monitors = %d; want 200", rec.Code)
	}
	var monitors []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &monitors); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	out := make(map[int64]bool, len(monitors))
	for _, m := range monitors {
		out[int64(m["id"].(float64))] = true
	}
	return out
}

// The headline behavior: a non-admin holding create_monitors can create one, and
// the thing they made shows up in their own list without an admin touching
// anything.
//
// The list assertion is the whole point. A 201 proves only that a row was
// written — the monitor could be invisible to the very user who just created it,
// which is exactly what happens if the auto-grant is dropped, and a status-code
// test would never notice.
func TestRBAC_Creator_CanCreateMonitorAndSeesItImmediately(t *testing.T) {
	h := newRBACHarness(t)

	id := h.createMonitorAs(t, h.creatorToken, "creator-made")

	if !h.monitorIDsVisibleTo(t, h.creatorToken)[id] {
		t.Fatal("the creator cannot see the monitor they just created; the auto-grant did not happen")
	}
	// It did not leak to an unrelated non-admin who was never granted it.
	if h.monitorIDsVisibleTo(t, h.memberToken)[id] {
		t.Error("another non-admin can see a monitor they were never granted")
	}
}

func TestRBAC_Creator_CanCreateOnlyInAllowedGroups(t *testing.T) {
	h := newRBACHarness(t)
	body := func(groupID any) map[string]any {
		return map[string]any{
			"name": "scoped", "type": "http", "group_id": groupID,
			"config": map[string]any{"url": "https://example.com"},
		}
	}

	before := len(h.monitorIDsVisibleTo(t, h.creatorToken))
	for name, groupID := range map[string]any{
		"top level":    nil,
		"hidden group": h.groupHidden,
	} {
		t.Run(name, func(t *testing.T) {
			rec := h.do(t, http.MethodPost, "/api/monitors", h.creatorToken, body(groupID))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("POST in %s = %d; want 403 (body: %s)", name, rec.Code, rec.Body.String())
			}
		})
	}
	if after := len(h.monitorIDsVisibleTo(t, h.creatorToken)); after != before {
		t.Fatalf("rejected creates changed visible monitor count %d -> %d", before, after)
	}

	allowed := h.do(t, http.MethodPost, "/api/monitors", h.creatorToken, body(h.groupVisible))
	if allowed.Code != http.StatusCreated {
		t.Fatalf("POST in granted group = %d; want 201 (body: %s)", allowed.Code, allowed.Body.String())
	}
	var created struct {
		ID      int64  `json:"id"`
		GroupID *int64 `json:"group_id"`
	}
	if err := json.Unmarshal(allowed.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal allowed create: %v", err)
	}
	move := h.do(t, http.MethodPut, "/api/monitors/"+intToStr(created.ID), h.creatorToken, map[string]any{
		"name": "scoped", "group_id": h.groupHidden,
	})
	if move.Code != http.StatusForbidden {
		t.Fatalf("PUT moving monitor to hidden group = %d; want 403", move.Code)
	}
	fetched := h.do(t, http.MethodGet, "/api/monitors/"+intToStr(created.ID), h.creatorToken, nil)
	if err := json.Unmarshal(fetched.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal monitor after rejected move: %v", err)
	}
	if created.GroupID == nil || *created.GroupID != h.groupVisible {
		t.Errorf("group after rejected move = %v; want %d", created.GroupID, h.groupVisible)
	}

	adminTopLevel := h.do(t, http.MethodPost, "/api/monitors", h.adminToken, body(nil))
	if adminTopLevel.Code != http.StatusCreated {
		t.Fatalf("admin POST at top level = %d; want 201 (body: %s)", adminTopLevel.Code, adminTopLevel.Body.String())
	}
}

// Top-level create is a separate permission choice for non-admins.
func TestRBAC_Creator_TopLevelRequiresExplicitCapability(t *testing.T) {
	h := newRBACHarness(t)
	ctx := context.Background()
	body := map[string]any{
		"name": "top-level", "type": "http", "group_id": nil,
		"config": map[string]any{"url": "https://example.com"},
	}

	denied := h.do(t, http.MethodPost, "/api/monitors", h.creatorToken, body)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("creator top-level without flag = %d; want 403 (body: %s)", denied.Code, denied.Body.String())
	}

	creator, err := h.users.GetByID(ctx, h.creatorID)
	if err != nil {
		t.Fatalf("load creator: %v", err)
	}
	creator.CanCreateTopLevelMonitors = true
	if err := h.users.Update(ctx, creator); err != nil {
		t.Fatalf("enable top-level: %v", err)
	}

	allowed := h.do(t, http.MethodPost, "/api/monitors", h.creatorToken, body)
	if allowed.Code != http.StatusCreated {
		t.Fatalf("creator top-level with flag = %d; want 201 (body: %s)", allowed.Code, allowed.Body.String())
	}
	var created struct {
		ID      int64  `json:"id"`
		GroupID *int64 `json:"group_id"`
	}
	if err := json.Unmarshal(allowed.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.GroupID != nil {
		t.Errorf("group_id = %v; want null top-level", created.GroupID)
	}
	if !h.monitorIDsVisibleTo(t, h.creatorToken)[created.ID] {
		t.Error("creator cannot see their top-level monitor after create")
	}
	if h.monitorIDsVisibleTo(t, h.memberToken)[created.ID] {
		t.Error("member saw a top-level monitor they were never granted")
	}
}

func TestRBAC_MonitorOwnerIsInformationalAndEditable(t *testing.T) {
	h := newRBACHarness(t)
	rec := h.do(t, http.MethodPost, "/api/monitors", h.creatorToken, map[string]any{
		"name": "owned-service", "owner": "Payments on-call", "type": "http",
		"group_id": h.groupCreator, "config": map[string]any{"url": "https://example.com"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST with owner = %d; want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var created struct {
		ID     int64  `json:"id"`
		Owner  string `json:"owner"`
		UserID int64  `json:"user_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	if created.Owner != "Payments on-call" {
		t.Errorf("owner = %q; want Payments on-call", created.Owner)
	}
	if created.UserID != h.creatorID {
		t.Errorf("created-by user_id = %d; want creator %d", created.UserID, h.creatorID)
	}

	updated := h.do(t, http.MethodPut, "/api/monitors/"+intToStr(created.ID), h.creatorToken, map[string]any{
		"name": "owned-service", "owner": "", "group_id": h.groupCreator,
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("PUT clearing owner = %d; want 200 (body: %s)", updated.Code, updated.Body.String())
	}
	var got struct {
		Owner  string `json:"owner"`
		UserID int64  `json:"user_id"`
	}
	if err := json.Unmarshal(updated.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal update: %v", err)
	}
	if got.Owner != "" || got.UserID != h.creatorID {
		t.Errorf("after clearing owner = %+v; informational owner must not change creator", got)
	}
}

// The capability gates CREATION only. Holding create_monitors must not hand a
// user edit rights over monitors somebody else made — including ones they can
// plainly see.
//
// This is the bypass worth guarding: the route has no admin middleware any more,
// so if requireMonitorEditAccess were dropped from the handler, every
// monitor-creating user could rename the whole install and this is the test that
// would fail.
func TestRBAC_Creator_CannotEditSomeoneElsesMonitor(t *testing.T) {
	h := newRBACHarness(t)
	visible := intToStr(h.monitorVisible)

	// Precondition: the creator really can see it. Without this the 403 below
	// would prove nothing — it could be a 404-shaped denial in disguise.
	if !h.monitorIDsVisibleTo(t, h.creatorToken)[h.monitorVisible] {
		t.Fatal("harness: the creator should be able to SEE the admin's monitor")
	}

	if rec := h.do(t, http.MethodPut, "/api/monitors/"+visible, h.creatorToken,
		map[string]any{"name": "renamed-by-creator"}); rec.Code != http.StatusForbidden {
		t.Errorf("PUT another user's monitor = %d; want 403", rec.Code)
	}
	if rec := h.do(t, http.MethodDelete, "/api/monitors/"+visible, h.creatorToken, nil); rec.Code != http.StatusForbidden {
		t.Errorf("DELETE another user's monitor = %d; want 403", rec.Code)
	}

	// Effect, not status: the monitor still exists under its original name.
	get := h.do(t, http.MethodGet, "/api/monitors/"+visible, h.creatorToken, nil)
	if get.Code != http.StatusOK {
		t.Fatalf("the admin's monitor is gone after a rejected DELETE: GET = %d", get.Code)
	}
	var monitor map[string]any
	if err := json.Unmarshal(get.Body.Bytes(), &monitor); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if monitor["name"] != "visible-monitor" {
		t.Errorf("monitor name = %v; a rejected PUT renamed it anyway", monitor["name"])
	}
}

// The other half of ownership: what you made, you may change and remove.
func TestRBAC_Creator_CanEditAndDeleteOwnMonitor(t *testing.T) {
	h := newRBACHarness(t)
	id := intToStr(h.createMonitorAs(t, h.creatorToken, "mine"))

	rec := h.do(t, http.MethodPut, "/api/monitors/"+id, h.creatorToken, map[string]any{
		"name": "mine-renamed", "group_id": h.groupCreator,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT own monitor = %d; want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	get := h.do(t, http.MethodGet, "/api/monitors/"+id, h.creatorToken, nil)
	var monitor map[string]any
	if err := json.Unmarshal(get.Body.Bytes(), &monitor); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if monitor["name"] != "mine-renamed" {
		t.Errorf("name = %v; the PUT returned 200 but did not rename it", monitor["name"])
	}

	if rec := h.do(t, http.MethodDelete, "/api/monitors/"+id, h.creatorToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE own monitor = %d; want 204", rec.Code)
	}
	if rec := h.do(t, http.MethodGet, "/api/monitors/"+id, h.creatorToken, nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d; want 404 — the 204 did not delete it", rec.Code)
	}
}

// An admin retains full control over what a non-admin created. Ownership adds a
// second key; it does not take the master key away.
func TestRBAC_Admin_CanEditMonitorCreatedByNonAdmin(t *testing.T) {
	h := newRBACHarness(t)
	id := intToStr(h.createMonitorAs(t, h.creatorToken, "creator-made"))

	if rec := h.do(t, http.MethodPut, "/api/monitors/"+id, h.adminToken,
		map[string]any{"name": "renamed-by-admin"}); rec.Code != http.StatusOK {
		t.Fatalf("admin PUT on a non-admin's monitor = %d; want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// Effect: a 200 only proves the authorization gate let the request through,
	// not that the edit landed. Re-fetch (fresh GET, not the PUT echo) to prove
	// the rename was actually persisted.
	getRec := h.do(t, http.MethodGet, "/api/monitors/"+id, h.adminToken, nil)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET after admin edit = %d; want 200 (body: %s)", getRec.Code, getRec.Body.String())
	}
	var fetched map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode post-edit get response: %v", err)
	}
	if fetched["name"] != "renamed-by-admin" {
		t.Errorf("name after admin edit = %v; want renamed-by-admin (PUT returned 200 but did not persist)", fetched["name"])
	}
}

// A user with no create capability is refused, and nothing is written. This is
// the negative that gives the positives above their meaning: creation is gated on
// the capability, not merely on being logged in.
func TestRBAC_NonCreator_CannotCreateMonitor(t *testing.T) {
	h := newRBACHarness(t)

	before := len(h.monitorIDsVisibleTo(t, h.memberToken))
	rec := h.do(t, http.MethodPost, "/api/monitors", h.memberToken, map[string]any{
		"name": "member-made", "type": "http", "config": map[string]any{"url": "https://example.com"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /api/monitors without the capability = %d; want 403", rec.Code)
	}
	if after := len(h.monitorIDsVisibleTo(t, h.memberToken)); after != before {
		t.Errorf("the member's monitor count went %d -> %d after a 403; the create happened anyway", before, after)
	}
}

// Metadata editors may change contact/condition on a granted group, but not
// rename, re-parent, or delete it.
func TestRBAC_GroupMetadataEditor_CanEditMetadataButNotStructureOrDelete(t *testing.T) {
	h := newRBACHarness(t)
	ctx := context.Background()

	// Reuse the creator principal (already granted the visible group) but drop
	// create/ownership powers and enable metadata-only edit.
	metaUser, err := h.users.GetByID(ctx, h.creatorID)
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	metaUser.CanCreateMonitors = false
	metaUser.CanCreateGroups = false
	metaUser.CanCreateTopLevelMonitors = false
	metaUser.CanEditGroupMetadata = true
	if err := h.users.Update(ctx, metaUser); err != nil {
		t.Fatalf("enable metadata: %v", err)
	}
	token := h.creatorToken

	ok := h.do(t, http.MethodPut, "/api/monitor-groups/"+intToStr(h.groupVisible), token, map[string]any{
		"name": "visible", "owner": "On-call desk", "description": "updated",
		"condition": "worst_of_children", "parent_id": nil,
	})
	if ok.Code != http.StatusOK {
		t.Fatalf("metadata PUT = %d; want 200 (body: %s)", ok.Code, ok.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(ok.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["owner"] != "On-call desk" || got["description"] != "updated" {
		t.Errorf("metadata not applied: %+v", got)
	}

	rename := h.do(t, http.MethodPut, "/api/monitor-groups/"+intToStr(h.groupVisible), token, map[string]any{
		"name": "hijacked", "owner": "On-call desk", "description": "updated",
		"condition": "worst_of_children", "parent_id": nil,
	})
	if rename.Code != http.StatusForbidden {
		t.Fatalf("rename as metadata editor = %d; want 403", rename.Code)
	}

	del := h.do(t, http.MethodDelete, "/api/monitor-groups/"+intToStr(h.groupVisible), token, nil)
	if del.Code != http.StatusForbidden {
		t.Fatalf("delete as metadata editor = %d; want 403", del.Code)
	}
}

// Groups: same shape as monitors. Create is capability-gated, the folder is
// visible to its creator at once, and editing someone else's is refused.
func TestRBAC_Creator_GroupCreateIsAutoGrantedAndOwnershipScoped(t *testing.T) {
	h := newRBACHarness(t)

	rec := h.do(t, http.MethodPost, "/api/monitor-groups", h.creatorToken, map[string]any{"name": "creator-folder"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/monitor-groups = %d; want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	id := intToStr(int64(created["id"].(float64)))

	// Visible to its creator without an admin granting anything.
	if got := h.do(t, http.MethodGet, "/api/monitor-groups/"+id, h.creatorToken, nil); got.Code != http.StatusOK {
		t.Errorf("GET own new folder = %d; want 200 — the auto-grant did not happen", got.Code)
	}
	// Editable by its creator.
	if got := h.do(t, http.MethodPut, "/api/monitor-groups/"+id, h.creatorToken,
		map[string]any{"name": "renamed"}); got.Code != http.StatusOK {
		t.Errorf("PUT own folder = %d; want 200", got.Code)
	}
	// But the admin's folder is off limits, even though the creator can see it.
	if got := h.do(t, http.MethodPut, "/api/monitor-groups/"+intToStr(h.groupVisible), h.creatorToken,
		map[string]any{"name": "hijacked"}); got.Code != http.StatusForbidden {
		t.Errorf("PUT another user's folder = %d; want 403", got.Code)
	}
	// And a user without create_groups is refused outright.
	if got := h.do(t, http.MethodPost, "/api/monitor-groups", h.memberToken,
		map[string]any{"name": "member-folder"}); got.Code != http.StatusForbidden {
		t.Errorf("POST without create_groups = %d; want 403", got.Code)
	}
}

// A monitor the caller cannot see must 404 on a mutation, not 403. A 403 would
// confirm that a monitor with that id exists in someone else's tenant — the
// existence leak requireMonitorEditAccess orders its two checks to avoid.
func TestRBAC_Creator_MutatingAnInvisibleMonitorIs404(t *testing.T) {
	h := newRBACHarness(t)
	hidden := intToStr(h.monitorHidden)

	if rec := h.do(t, http.MethodPut, "/api/monitors/"+hidden, h.creatorToken,
		map[string]any{"name": "peek"}); rec.Code != http.StatusNotFound {
		t.Errorf("PUT an invisible monitor = %d; want 404 (403 would confirm it exists)", rec.Code)
	}
	if rec := h.do(t, http.MethodDelete, "/api/monitors/"+hidden, h.creatorToken, nil); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE an invisible monitor = %d; want 404", rec.Code)
	}
}

// --- Tests: tags on the monitor wire format --------------------------------

// The dashboard's tag filter reads monitor.tags. It must be present on both the
// list and the single-monitor read, and must be [] — never null — for a monitor
// with no tags, or every consumer needs a null check.
func TestRBAC_MonitorWireFormat_CarriesTags(t *testing.T) {
	h := newRBACHarness(t)

	rec := h.do(t, http.MethodGet, "/api/monitors", h.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/monitors = %d", rec.Code)
	}

	// Decode into a shape that distinguishes "missing" from "null" from "[]".
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(raw) != 2 {
		t.Fatalf("admin sees %d monitors; want 2", len(raw))
	}

	for _, m := range raw {
		tagsRaw, ok := m["tags"]
		if !ok {
			t.Fatal(`monitor JSON has no "tags" field`)
		}
		if string(tagsRaw) == "null" {
			t.Fatal(`"tags" serialized as null; it must always be an array`)
		}
	}

	// The tagged monitor carries the full tag shape: id / name / color / value.
	var monitors []struct {
		ID   int64 `json:"id"`
		Tags []struct {
			ID    int64  `json:"id"`
			Name  string `json:"name"`
			Color string `json:"color"`
			Value string `json:"value"`
		} `json:"tags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &monitors); err != nil {
		t.Fatalf("unmarshal typed: %v", err)
	}
	for _, m := range monitors {
		switch m.ID {
		case h.monitorVisible:
			if len(m.Tags) != 1 {
				t.Fatalf("tagged monitor carries %d tags; want 1", len(m.Tags))
			}
			got := m.Tags[0]
			if got.ID != h.tagID || got.Name != "prod" || got.Color != "#ff0000" || got.Value != "eu-west" {
				t.Errorf("tag = %+v; want {id:%d name:prod color:#ff0000 value:eu-west}", got, h.tagID)
			}
		case h.monitorHidden:
			if len(m.Tags) != 0 {
				t.Errorf("untagged monitor carries %d tags; want 0", len(m.Tags))
			}
		}
	}

	// And on the single-monitor read.
	one := h.do(t, http.MethodGet, "/api/monitors/"+intToStr(h.monitorVisible), h.adminToken, nil)
	var single struct {
		Tags []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"tags"`
	}
	if err := json.Unmarshal(one.Body.Bytes(), &single); err != nil {
		t.Fatalf("unmarshal single: %v", err)
	}
	if len(single.Tags) != 1 || single.Tags[0].Name != "prod" || single.Tags[0].Value != "eu-west" {
		t.Errorf("GET /api/monitors/:id tags = %+v; want the prod/eu-west tag", single.Tags)
	}
}
