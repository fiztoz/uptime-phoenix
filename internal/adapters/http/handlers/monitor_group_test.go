// Package handlers_test contains integration tests for HTTP handlers.
package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

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

// --- In-memory monitor group repo for tests -------------------------------

type fakeMonitorGroupRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.MonitorGroup
	nextID int64
}

func newFakeMonitorGroupRepo() *fakeMonitorGroupRepo {
	return &fakeMonitorGroupRepo{byID: make(map[int64]*domain.MonitorGroup)}
}

func (r *fakeMonitorGroupRepo) Create(_ context.Context, g *domain.MonitorGroup) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	g.ID = r.nextID
	now := time.Now().UTC()
	g.CreatedAt = now
	g.UpdatedAt = now
	cp := *g
	r.byID[g.ID] = &cp
	return nil
}

func (r *fakeMonitorGroupRepo) GetByID(_ context.Context, id int64) (*domain.MonitorGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *g
	return &cp, nil
}

func (r *fakeMonitorGroupRepo) List(_ context.Context, userID int64) ([]*domain.MonitorGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.MonitorGroup, 0, len(r.byID))
	for _, g := range r.byID {
		if g.UserID == userID {
			cp := *g
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ListAll returns every group regardless of owner — the RBAC listing path.
func (r *fakeMonitorGroupRepo) ListAll(_ context.Context) ([]*domain.MonitorGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.MonitorGroup, 0, len(r.byID))
	for _, g := range r.byID {
		cp := *g
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *fakeMonitorGroupRepo) Update(_ context.Context, g *domain.MonitorGroup) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.byID[g.ID]
	if !ok {
		return ports.ErrNotFound
	}
	g.UpdatedAt = time.Now().UTC()
	cp := *g
	// last_status is owned by ClaimStatusTransition; the real repos exclude it from
	// Update so an admin PUT cannot clobber a worker's alerting decision.
	cp.LastStatus = existing.LastStatus
	r.byID[g.ID] = &cp
	return nil
}

// ClaimStatusTransition models the repositories' compare-and-set: the write lands
// only when the stored value still matches `from`. Modeling it as an
// unconditional write would make the fake claim every race, which is precisely
// the failure the CAS exists to prevent.
func (r *fakeMonitorGroupRepo) ClaimStatusTransition(_ context.Context, groupID int64, from *domain.Status, to domain.Status) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.byID[groupID]
	if !ok {
		return false, ports.ErrNotFound
	}
	// Null-safe comparison — the `IS` / `<=>` the SQL adapters use.
	switch {
	case g.LastStatus == nil && from == nil:
	case g.LastStatus == nil || from == nil:
		return false, nil
	case *g.LastStatus != *from:
		return false, nil
	}
	next := to
	g.LastStatus = &next
	return true, nil
}

// Delete removes the group only, exactly like the real repositories must:
// it never touches monitors, proving deletion doesn't cascade.
func (r *fakeMonitorGroupRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[id]; !ok {
		return ports.ErrNotFound
	}
	delete(r.byID, id)
	return nil
}

// --- Minimal in-memory heartbeat repo for tests ---------------------------
//
// Only GetLatest/Save are exercised by MonitorGroupService.ResolveStatuses;
// the rest of ports.HeartbeatRepository is implemented as no-ops so this type
// satisfies the interface.
type fakeGroupHeartbeatRepo struct {
	mu     sync.Mutex
	latest map[int64]*domain.Heartbeat
}

func newFakeGroupHeartbeatRepo() *fakeGroupHeartbeatRepo {
	return &fakeGroupHeartbeatRepo{latest: make(map[int64]*domain.Heartbeat)}
}

func (r *fakeGroupHeartbeatRepo) Save(_ context.Context, h *domain.Heartbeat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latest[h.MonitorID] = h
	return nil
}

func (r *fakeGroupHeartbeatRepo) GetLatest(_ context.Context, monitorID int64) (*domain.Heartbeat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.latest[monitorID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return h, nil
}

func (r *fakeGroupHeartbeatRepo) ListByMonitor(_ context.Context, _ int64, _, _ time.Time) ([]*domain.Heartbeat, error) {
	return nil, nil
}
func (r *fakeGroupHeartbeatRepo) DeleteByMonitor(_ context.Context, _ int64) error     { return nil }
func (r *fakeGroupHeartbeatRepo) DeleteOlderThan(_ context.Context, _ time.Time) error { return nil }
func (r *fakeGroupHeartbeatRepo) SaveAggregate1m(_ context.Context, _ *ports.Aggregate1m) error {
	return nil
}
func (r *fakeGroupHeartbeatRepo) SaveAggregate1h(_ context.Context, _ *ports.Aggregate1h) error {
	return nil
}
func (r *fakeGroupHeartbeatRepo) SaveAggregate1d(_ context.Context, _ *ports.Aggregate1d) error {
	return nil
}
func (r *fakeGroupHeartbeatRepo) GetAggregate1m(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1m, error) {
	return nil, nil
}
func (r *fakeGroupHeartbeatRepo) GetAggregate1h(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1h, error) {
	return nil, nil
}
func (r *fakeGroupHeartbeatRepo) GetAggregate1d(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1d, error) {
	return nil, nil
}

// --- Test harness -----------------------------------------------------------

type monitorGroupTestHarness struct {
	router      *echo.Echo
	monitorRepo *fakeMonitorRepo
	groupRepo   *fakeMonitorGroupRepo
	hbRepo      *fakeGroupHeartbeatRepo
	token       string
}

func newMonitorGroupHarness(t *testing.T) *monitorGroupTestHarness {
	t.Helper()

	userRepo := memory.NewUserRepo()
	apiKeyRepo := memory.NewAPIKeyRepo()
	authenticator := auth.NewJWTAuthenticator("test-key", 24, userRepo)
	totp := auth.NewTOTPProvider("Phoenix")
	authSvc := services.NewAuthService(userRepo, apiKeyRepo, authenticator, totp)

	ctx := context.Background()
	if _, err := authSvc.Register(ctx, "groupuser", "password123"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, err := authSvc.Login(ctx, "groupuser", "password123")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	monitorRepo := newFakeMonitorRepo()
	bus := newFakeMonitorBus()
	monitorSvc := services.NewMonitorService(monitorRepo, bus)

	groupRepo := newFakeMonitorGroupRepo()
	hbRepo := newFakeGroupHeartbeatRepo()
	groupSvc := services.NewMonitorGroupService(groupRepo, monitorRepo, hbRepo, logger.New("error"))

	// A monitor's group_id is only accepted once group ownership can be
	// validated (mirrors SetProxyRepo) — wire it so Create/Update with a
	// real group_id succeeds.
	monitorSvc.SetGroupRepo(groupRepo)

	// The harness user is the bootstrap user, hence an admin: this file asserts the
	// single-admin install still behaves exactly as it did before RBAC.
	accessSvc := services.NewAccessService(userRepo, memory.NewUserPermissionRepo(), groupRepo, monitorRepo)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	monitorH := handlers.NewMonitorHandlers(monitorSvc, accessSvc, nil, nil)
	monGroup := e.Group("/api/monitors", middleware.AuthMiddleware(authSvc))
	monGroup.POST("", monitorH.Create)
	monGroup.GET("", monitorH.List)
	monGroup.GET("/:id", monitorH.GetByID)
	monGroup.PUT("/:id", monitorH.Update)
	monGroup.DELETE("/:id", monitorH.Delete)

	groupH := handlers.NewMonitorGroupHandlers(groupSvc, accessSvc)
	groupsGroup := e.Group("/api/monitor-groups", middleware.AuthMiddleware(authSvc))
	groupsGroup.POST("", groupH.Create)
	groupsGroup.GET("", groupH.List)
	groupsGroup.GET("/:id", groupH.GetByID)
	groupsGroup.PUT("/:id", groupH.Update)
	groupsGroup.DELETE("/:id", groupH.Delete)

	return &monitorGroupTestHarness{
		router:      e,
		monitorRepo: monitorRepo,
		groupRepo:   groupRepo,
		hbRepo:      hbRepo,
		token:       token,
	}
}

func (h *monitorGroupTestHarness) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
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
	req.Header.Set(echo.HeaderAuthorization, "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

// --- Tests -------------------------------------------------------------

func TestMonitorGroupHandlers_Create(t *testing.T) {
	h := newMonitorGroupHarness(t)

	body := map[string]any{
		"name":      "Payments API",
		"condition": "worst_of_children",
	}
	rec := h.do(t, http.MethodPost, "/api/monitor-groups", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/monitor-groups returned %d; want 201 (body: %s)", rec.Code, rec.Body.String())
	}

	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if view["name"] != "Payments API" {
		t.Errorf("name = %v; want Payments API", view["name"])
	}
	if view["condition"] != "worst_of_children" {
		t.Errorf("condition = %v; want worst_of_children", view["condition"])
	}
	// status must be present and explicitly null on create (never omitted —
	// see MonitorGroupView.Status), not just absent from the JSON object.
	statusVal, hasStatus := view["status"]
	if !hasStatus {
		t.Fatalf("response missing status key entirely; want present with null value")
	}
	if statusVal != nil {
		t.Errorf("status on create = %v; want null (no children yet)", statusVal)
	}
}

func TestMonitorGroupHandlers_Create_MissingName(t *testing.T) {
	h := newMonitorGroupHarness(t)

	body := map[string]any{"condition": "worst_of_children"}
	rec := h.do(t, http.MethodPost, "/api/monitor-groups", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /api/monitor-groups (no name) returned %d; want 400", rec.Code)
	}
}

// TestMonitorGroupHandlers_List_DerivedStatus asserts the effect described in
// the contract: LIST must carry a non-null derived status for a group that
// has a resolvable child, and null for an "ignore" group — never just a 2xx.
func TestMonitorGroupHandlers_List_DerivedStatus(t *testing.T) {
	h := newMonitorGroupHarness(t)

	// Group with a child monitor that is UP.
	groupRec := h.do(t, http.MethodPost, "/api/monitor-groups", map[string]any{
		"name":      "Has Children",
		"condition": "worst_of_children",
	})
	if groupRec.Code != http.StatusCreated {
		t.Fatalf("create group: %d (%s)", groupRec.Code, groupRec.Body.String())
	}
	var group map[string]any
	if err := json.Unmarshal(groupRec.Body.Bytes(), &group); err != nil {
		t.Fatalf("decode group: %v", err)
	}
	groupID := int64(group["id"].(float64))

	// Ignore-condition group: must never carry a status regardless of children.
	ignoreRec := h.do(t, http.MethodPost, "/api/monitor-groups", map[string]any{
		"name":      "Just A Folder",
		"condition": "ignore",
	})
	if ignoreRec.Code != http.StatusCreated {
		t.Fatalf("create ignore group: %d (%s)", ignoreRec.Code, ignoreRec.Body.String())
	}
	var ignoreGroup map[string]any
	if err := json.Unmarshal(ignoreRec.Body.Bytes(), &ignoreGroup); err != nil {
		t.Fatalf("decode ignore group: %v", err)
	}
	ignoreGroupID := int64(ignoreGroup["id"].(float64))

	// Monitor filed under the first group, with an UP heartbeat.
	monRec := h.do(t, http.MethodPost, "/api/monitors", map[string]any{
		"name":     "Child Monitor",
		"type":     "http",
		"config":   map[string]any{"url": "https://example.com"},
		"group_id": groupID,
	})
	if monRec.Code != http.StatusCreated {
		t.Fatalf("create monitor: %d (%s)", monRec.Code, monRec.Body.String())
	}
	var mon map[string]any
	if err := json.Unmarshal(monRec.Body.Bytes(), &mon); err != nil {
		t.Fatalf("decode monitor: %v", err)
	}
	monitorID := int64(mon["id"].(float64))

	// Without a heartbeat ResolveStatuses would have nothing to roll up, so
	// seed one directly — this is what makes the group's status resolvable.
	if err := h.hbRepo.Save(context.Background(), &domain.Heartbeat{
		MonitorID: monitorID,
		Status:    domain.StatusUp,
		Time:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}

	rec := h.do(t, http.MethodGet, "/api/monitor-groups", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/monitor-groups returned %d; want 200", rec.Code)
	}
	var views []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatalf("decode list: %v", err)
	}

	var gotGroup, gotIgnore map[string]any
	for _, v := range views {
		switch int64(v["id"].(float64)) {
		case groupID:
			gotGroup = v
		case ignoreGroupID:
			gotIgnore = v
		}
	}
	if gotGroup == nil {
		t.Fatalf("group with children missing from list response")
	}
	if gotIgnore == nil {
		t.Fatalf("ignore group missing from list response")
	}

	// The group with an UP child must resolve to a real, non-null status —
	// domain.StatusUp (1) per domain.MonitorGroup.Rollup's worst_of_children
	// rule with a single non-maintenance UP child.
	status, ok := gotGroup["status"].(float64)
	if !ok {
		t.Fatalf("status for group-with-children = %v (%T); want a non-null number", gotGroup["status"], gotGroup["status"])
	}
	if int(status) != int(domain.StatusUp) {
		t.Errorf("status for group-with-children = %v; want %d (StatusUp)", status, domain.StatusUp)
	}

	// The ignore-condition group must never carry a derived status.
	if v, present := gotIgnore["status"]; !present {
		t.Fatalf("response missing status key for ignore group entirely; want present with null value")
	} else if v != nil {
		t.Errorf("status for ignore group = %v; want null", v)
	}
}

// TestMonitorGroupHandlers_Delete_DoesNotDeleteMonitors asserts the effect
// required by the contract: deleting a group must return 204 AND must not
// delete the monitors that were filed under it — a 204 alone proves nothing,
// since a handler wired to the wrong service call could easily cascade-delete.
func TestMonitorGroupHandlers_Delete_DoesNotDeleteMonitors(t *testing.T) {
	h := newMonitorGroupHarness(t)

	groupRec := h.do(t, http.MethodPost, "/api/monitor-groups", map[string]any{
		"name":      "To Delete",
		"condition": "worst_of_children",
	})
	if groupRec.Code != http.StatusCreated {
		t.Fatalf("create group: %d (%s)", groupRec.Code, groupRec.Body.String())
	}
	var group map[string]any
	if err := json.Unmarshal(groupRec.Body.Bytes(), &group); err != nil {
		t.Fatalf("decode group: %v", err)
	}
	groupID := int64(group["id"].(float64))

	monRec := h.do(t, http.MethodPost, "/api/monitors", map[string]any{
		"name":     "Monitor In Group",
		"type":     "http",
		"config":   map[string]any{"url": "https://example.com"},
		"group_id": groupID,
	})
	if monRec.Code != http.StatusCreated {
		t.Fatalf("create monitor: %d (%s)", monRec.Code, monRec.Body.String())
	}
	var mon map[string]any
	if err := json.Unmarshal(monRec.Body.Bytes(), &mon); err != nil {
		t.Fatalf("decode monitor: %v", err)
	}
	monitorID := int64(mon["id"].(float64))

	delRec := h.do(t, http.MethodDelete, "/api/monitor-groups/"+intToStr(groupID), nil)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /api/monitor-groups/:id returned %d; want 204", delRec.Code)
	}

	// The group itself must be gone.
	getGroupRec := h.do(t, http.MethodGet, "/api/monitor-groups/"+intToStr(groupID), nil)
	if getGroupRec.Code != http.StatusNotFound {
		t.Errorf("GET deleted group returned %d; want 404", getGroupRec.Code)
	}

	// But the monitor that was filed under it must still exist — deleting a
	// folder must never delete the monitors inside it.
	getMonRec := h.do(t, http.MethodGet, "/api/monitors/"+intToStr(monitorID), nil)
	if getMonRec.Code != http.StatusOK {
		t.Fatalf("monitor that was in the deleted group returned %d on GET; want 200 (monitor must survive)", getMonRec.Code)
	}
}

// TestMonitorGroupHandlers_Update_ClearsParent proves PUT .../:id with
// parent_id: null actually un-nests a group, mirroring the group_id
// clear-semantics test for monitors — a bare 200 proves nothing.
func TestMonitorGroupHandlers_Update_ClearsParent(t *testing.T) {
	h := newMonitorGroupHarness(t)

	parentRec := h.do(t, http.MethodPost, "/api/monitor-groups", map[string]any{
		"name":      "Parent",
		"condition": "worst_of_children",
	})
	if parentRec.Code != http.StatusCreated {
		t.Fatalf("create parent: %d (%s)", parentRec.Code, parentRec.Body.String())
	}
	var parent map[string]any
	if err := json.Unmarshal(parentRec.Body.Bytes(), &parent); err != nil {
		t.Fatalf("decode parent: %v", err)
	}
	parentID := int64(parent["id"].(float64))

	childRec := h.do(t, http.MethodPost, "/api/monitor-groups", map[string]any{
		"name":      "Child",
		"condition": "worst_of_children",
		"parent_id": parentID,
	})
	if childRec.Code != http.StatusCreated {
		t.Fatalf("create child: %d (%s)", childRec.Code, childRec.Body.String())
	}
	var child map[string]any
	if err := json.Unmarshal(childRec.Body.Bytes(), &child); err != nil {
		t.Fatalf("decode child: %v", err)
	}
	childID := int64(child["id"].(float64))
	if child["parent_id"] == nil {
		t.Fatalf("test setup: child's parent_id should be non-nil after creation")
	}

	updateRec := h.do(t, http.MethodPut, "/api/monitor-groups/"+intToStr(childID), map[string]any{
		"name":      "Child",
		"condition": "worst_of_children",
		"parent_id": nil,
	})
	if updateRec.Code != http.StatusOK {
		t.Fatalf("PUT /api/monitor-groups/:id returned %d; want 200 (%s)", updateRec.Code, updateRec.Body.String())
	}

	// Re-fetch with a fresh GET to prove persistence, not just an echo.
	getRec := h.do(t, http.MethodGet, "/api/monitor-groups/"+intToStr(childID), nil)
	var after map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode post-clear get: %v", err)
	}
	if after["parent_id"] != nil {
		t.Errorf("parent_id after clearing PUT = %v; want nil", after["parent_id"])
	}
}
