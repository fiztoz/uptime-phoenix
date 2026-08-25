// Package handlers_test contains integration tests for HTTP handlers.
package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/middleware"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/scheduler"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// --- In-memory maintenance window repo for tests -------------------------

type fakeMaintenanceRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.MaintenanceWindow
	nextID int64
}

func newFakeMaintenanceRepo() *fakeMaintenanceRepo {
	return &fakeMaintenanceRepo{byID: make(map[int64]*domain.MaintenanceWindow)}
}

func (r *fakeMaintenanceRepo) Create(_ context.Context, mw *domain.MaintenanceWindow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	mw.ID = r.nextID
	cp := *mw
	r.byID[mw.ID] = &cp
	return nil
}

func (r *fakeMaintenanceRepo) GetByID(_ context.Context, id int64) (*domain.MaintenanceWindow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	mw, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *mw
	return &cp, nil
}

func (r *fakeMaintenanceRepo) List(_ context.Context, userID int64) ([]*domain.MaintenanceWindow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.MaintenanceWindow, 0)
	for _, mw := range r.byID {
		if userID > 0 && mw.UserID != userID {
			continue
		}
		cp := *mw
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *fakeMaintenanceRepo) ListAll(ctx context.Context) ([]*domain.MaintenanceWindow, error) {
	return r.List(ctx, 0)
}

func (r *fakeMaintenanceRepo) Update(_ context.Context, mw *domain.MaintenanceWindow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[mw.ID]; !ok {
		return ports.ErrNotFound
	}
	cp := *mw
	r.byID[mw.ID] = &cp
	return nil
}

func (r *fakeMaintenanceRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[id]; !ok {
		return ports.ErrNotFound
	}
	delete(r.byID, id)
	return nil
}

// fakeMaintenanceLinkRepo is an in-memory ports.MaintenanceWindowMonitorRepository.
// Alert suppression depends entirely on these links existing, so they are
// modeled for real.
type fakeMaintenanceLinkRepo struct {
	mu    sync.Mutex
	links map[int64][]int64 // maintenanceID -> monitorIDs
	repo  *fakeMaintenanceRepo
}

func newFakeMaintenanceLinkRepo(repo *fakeMaintenanceRepo) *fakeMaintenanceLinkRepo {
	return &fakeMaintenanceLinkRepo{links: make(map[int64][]int64), repo: repo}
}

func (r *fakeMaintenanceLinkRepo) Assign(_ context.Context, maintenanceID, monitorID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range r.links[maintenanceID] {
		if id == monitorID {
			return nil // already linked — assign is idempotent
		}
	}
	r.links[maintenanceID] = append(r.links[maintenanceID], monitorID)
	return nil
}

func (r *fakeMaintenanceLinkRepo) Remove(_ context.Context, maintenanceID, monitorID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.links[maintenanceID]
	out := make([]int64, 0, len(cur))
	for _, id := range cur {
		if id != monitorID {
			out = append(out, id)
		}
	}
	r.links[maintenanceID] = out
	return nil
}

func (r *fakeMaintenanceLinkRepo) ListByMaintenance(_ context.Context, maintenanceID int64) ([]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.links[maintenanceID]...), nil
}

func (r *fakeMaintenanceLinkRepo) ListByMonitor(ctx context.Context, monitorID int64) ([]*domain.MaintenanceWindow, error) {
	r.mu.Lock()
	ids := make([]int64, 0)
	for maintID, monitorIDs := range r.links {
		for _, id := range monitorIDs {
			if id == monitorID {
				ids = append(ids, maintID)
				break
			}
		}
	}
	r.mu.Unlock()

	out := make([]*domain.MaintenanceWindow, 0, len(ids))
	for _, id := range ids {
		mw, err := r.repo.GetByID(ctx, id)
		if err != nil {
			continue
		}
		out = append(out, mw)
	}
	return out, nil
}

// --- Test harness ---------------------------------------------------------

// maintenanceTestHarness models the three principals the RBAC maintenance rules
// distinguish, because ownership is no longer one of them:
//
//	A — admin. Sees every monitor and every window; may do anything.
//	B — non-admin holding can_manage_maintenance, granted monitor B ONLY. May
//	    create/edit/delete ANY window (they are install-wide objects), but may not
//	    point one at a monitor they cannot see.
//	C — non-admin with NO capability, granted monitor B only. Read-only, and only
//	    for windows that cover monitor B.
type maintenanceTestHarness struct {
	router *echo.Echo
	tokenA string // admin
	tokenB string // can_manage_maintenance, granted monitor B
	tokenC string // no capability, granted monitor B

	maintSvc   *services.MaintenanceService
	monitorAID int64 // visible to A only
	monitorBID int64 // visible to A, B and C
}

func newMaintenanceHarness(t *testing.T) *maintenanceTestHarness {
	t.Helper()

	userRepo := memory.NewUserRepo()
	apiKeyRepo := memory.NewAPIKeyRepo()
	permRepo := memory.NewUserPermissionRepo()
	authenticator := auth.NewJWTAuthenticator("test-key", 24, userRepo)
	totp := auth.NewTOTPProvider("Phoenix")
	authSvc := services.NewAuthService(userRepo, apiKeyRepo, authenticator, totp)

	ctx := context.Background()

	// User A: bootstrap user via Register (first user) — always an admin.
	if _, err := authSvc.Register(ctx, "maint-user-a", "password123"); err != nil {
		t.Fatalf("register user A: %v", err)
	}
	tokenA, err := authSvc.Login(ctx, "maint-user-a", "password123")
	if err != nil {
		t.Fatalf("login user A: %v", err)
	}

	// Users B and C: created via the admin-supplied path (mirrors POST /api/users;
	// self-registration only ever bootstraps the first user).
	if _, err := authSvc.CreateUser(ctx, "maint-user-b", "password123", true, false, "UTC",
		services.UserCapabilities{CanManageMaintenance: true}); err != nil {
		t.Fatalf("create user B: %v", err)
	}
	tokenB, err := authSvc.Login(ctx, "maint-user-b", "password123")
	if err != nil {
		t.Fatalf("login user B: %v", err)
	}
	if _, err := authSvc.CreateUser(ctx, "maint-user-c", "password123", true, false, "UTC",
		services.UserCapabilities{}); err != nil {
		t.Fatalf("create user C: %v", err)
	}
	tokenC, err := authSvc.Login(ctx, "maint-user-c", "password123")
	if err != nil {
		t.Fatalf("login user C: %v", err)
	}

	maintRepo := newFakeMaintenanceRepo()
	linkRepo := newFakeMaintenanceLinkRepo(maintRepo)
	cronEval := scheduler.NewCronEvaluator()
	maintenanceSvc := services.NewMaintenanceService(maintRepo, linkRepo, cronEval)

	// Two monitors. Both are created by the admin — under RBAC "who owns it" no
	// longer decides anything; the GRANTS do.
	monitorRepo := newFakeMonitorRepo()
	monitorA := &domain.Monitor{UserID: 1, Name: "a-monitor", Type: "http", Active: true, Interval: 60}
	monitorB := &domain.Monitor{UserID: 1, Name: "b-monitor", Type: "http", Active: true, Interval: 60}
	if err := monitorRepo.Create(ctx, monitorA); err != nil {
		t.Fatalf("create monitor A: %v", err)
	}
	if err := monitorRepo.Create(ctx, monitorB); err != nil {
		t.Fatalf("create monitor B: %v", err)
	}

	// B (id 2) and C (id 3) can see monitor B, and nothing else.
	if err := permRepo.Grant(ctx, &domain.UserPermission{UserID: 2, MonitorID: &monitorB.ID}); err != nil {
		t.Fatalf("grant monitor B to user B: %v", err)
	}
	if err := permRepo.Grant(ctx, &domain.UserPermission{UserID: 3, MonitorID: &monitorB.ID}); err != nil {
		t.Fatalf("grant monitor B to user C: %v", err)
	}

	accessSvc := services.NewAccessService(userRepo, permRepo, nil, monitorRepo)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	maintH := handlers.NewMaintenanceHandlers(maintenanceSvc, accessSvc)
	maintGroup := e.Group("/api/maintenance", middleware.AuthMiddleware(authSvc))
	maintGroup.POST("", maintH.Create)
	maintGroup.GET("", maintH.List)
	maintGroup.GET("/:id", maintH.Get)
	maintGroup.PUT("/:id", maintH.Update)
	maintGroup.DELETE("/:id", maintH.Delete)

	maintMonGroup := e.Group("/api/maintenance/:id/monitors", middleware.AuthMiddleware(authSvc))
	maintMonGroup.POST("", maintH.AssignMonitor)
	maintMonGroup.DELETE("/:monitor_id", maintH.UnassignMonitor)
	maintMonGroup.GET("", maintH.ListMonitors)

	return &maintenanceTestHarness{
		router:     e,
		tokenA:     tokenA,
		tokenB:     tokenB,
		tokenC:     tokenC,
		maintSvc:   maintenanceSvc,
		monitorAID: monitorA.ID,
		monitorBID: monitorB.ID,
	}
}

func (h *maintenanceTestHarness) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
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

// --- Tests -----------------------------------------------------------------

// TestMaintenanceHandlers_Create_SetsUserIDFromContext asserts the created
// window's UserID comes from the authenticated principal, not a caller-supplied
// or zero value.
func TestMaintenanceHandlers_Create_SetsUserIDFromContext(t *testing.T) {
	h := newMaintenanceHarness(t)

	body := map[string]any{
		"title":       "Cron Maintenance",
		"description": "Recurring maintenance with no fixed dates.",
		"strategy":    "cron",
		"cron_expr":   "0 2 * * *",
		"duration":    30,
	}
	rec := h.do(t, http.MethodPost, "/api/maintenance", h.tokenA, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/maintenance returned %d; want 201 (body: %s)", rec.Code, rec.Body.String())
	}

	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	// domain.MaintenanceWindow has no json tags, so the wire shape uses the
	// The wire shape is snake_case (MaintenanceView) — never the raw domain struct.
	userID, ok := created["user_id"].(float64)
	if !ok || userID <= 0 {
		t.Fatalf("user_id = %v; want a positive number set from the authenticated context", created["user_id"])
	}
}

// TestMaintenanceHandlers_Create_Unauthenticated asserts Create requires an
// authenticated principal (no bearer token = 401), same convention as
// MonitorHandlers.Create.
func TestMaintenanceHandlers_Create_Unauthenticated(t *testing.T) {
	h := newMaintenanceHarness(t)

	body := map[string]any{"title": "No Auth", "strategy": "single"}
	rec := h.do(t, http.MethodPost, "/api/maintenance", "", body)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/maintenance (no token) returned %d; want 401", rec.Code)
	}
}

// TestMaintenanceHandlers_List_CapabilityHolderSeesEveryWindow: a maintenance
// window is an install-wide object. A can_manage_maintenance holder who could not
// see the windows the admin created would hold a useless grant, so their list is
// the FULL list — not "the ones I happen to have created".
func TestMaintenanceHandlers_List_CapabilityHolderSeesEveryWindow(t *testing.T) {
	h := newMaintenanceHarness(t)

	for _, title := range []string{"A-1", "A-2"} {
		body := map[string]any{"title": title, "strategy": "single"}
		rec := h.do(t, http.MethodPost, "/api/maintenance", h.tokenA, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d (%s)", title, rec.Code, rec.Body.String())
		}
	}

	listRec := h.do(t, http.MethodGet, "/api/maintenance", h.tokenB, nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /api/maintenance (capability holder) returned %d", listRec.Code)
	}
	var windows []map[string]any
	if err := json.Unmarshal(listRec.Body.Bytes(), &windows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(windows) != 2 {
		t.Errorf("capability holder sees %d windows; want 2 (the admin's windows must be manageable)", len(windows))
	}
}

// TestMaintenanceHandlers_List_ReadOnlyUserSeesOnlyWindowsOnVisibleMonitors: a
// non-admin WITHOUT the capability gets a read-only list restricted to windows
// that cover monitors they can actually see. A 200 with the wrong rows is the
// failure mode here, so the assertion is on the CONTENT, not the status.
func TestMaintenanceHandlers_List_ReadOnlyUserSeesOnlyWindowsOnVisibleMonitors(t *testing.T) {
	h := newMaintenanceHarness(t)

	// Admin creates one window over monitor A (invisible to C) and one over
	// monitor B (visible to C).
	hidden := h.do(t, http.MethodPost, "/api/maintenance", h.tokenA, map[string]any{
		"title": "covers-A", "strategy": "single", "monitor_ids": []int64{h.monitorAID},
	})
	if hidden.Code != http.StatusCreated {
		t.Fatalf("create covers-A: %d (%s)", hidden.Code, hidden.Body.String())
	}
	shown := h.do(t, http.MethodPost, "/api/maintenance", h.tokenA, map[string]any{
		"title": "covers-B", "strategy": "single", "monitor_ids": []int64{h.monitorBID},
	})
	if shown.Code != http.StatusCreated {
		t.Fatalf("create covers-B: %d (%s)", shown.Code, shown.Body.String())
	}

	listRec := h.do(t, http.MethodGet, "/api/maintenance", h.tokenC, nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /api/maintenance (read-only user) returned %d", listRec.Code)
	}
	var windows []map[string]any
	if err := json.Unmarshal(listRec.Body.Bytes(), &windows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("read-only user sees %d windows; want 1", len(windows))
	}
	if windows[0]["title"] != "covers-B" {
		t.Errorf("read-only user's list = %v; leaked a window over a monitor they cannot see", windows[0]["title"])
	}
}

// TestMaintenanceHandlers_Get_HiddenFromUserWithoutAccess: a window covering a
// monitor the caller cannot see must be indistinguishable from one that does not
// exist — 404, never 403.
func TestMaintenanceHandlers_Get_HiddenFromUserWithoutAccess(t *testing.T) {
	h := newMaintenanceHarness(t)

	createRec := h.do(t, http.MethodPost, "/api/maintenance", h.tokenA, map[string]any{
		"title": "A-Private", "strategy": "single", "monitor_ids": []int64{h.monitorAID},
	})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", createRec.Code, createRec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	id := floatToIntStr(created["id"].(float64))

	// The admin can fetch it.
	ownRec := h.do(t, http.MethodGet, "/api/maintenance/"+id, h.tokenA, nil)
	if ownRec.Code != http.StatusOK {
		t.Errorf("admin GET /api/maintenance/%s returned %d; want 200", id, ownRec.Code)
	}

	// The read-only user, who cannot see monitor A, must not.
	otherRec := h.do(t, http.MethodGet, "/api/maintenance/"+id, h.tokenC, nil)
	if otherRec.Code != http.StatusNotFound {
		t.Errorf("read-only user GET /api/maintenance/%s returned %d; want 404", id, otherRec.Code)
	}

	// The capability holder MAY see it: managing maintenance is install-wide.
	capRec := h.do(t, http.MethodGet, "/api/maintenance/"+id, h.tokenB, nil)
	if capRec.Code != http.StatusOK {
		t.Errorf("capability holder GET /api/maintenance/%s returned %d; want 200", id, capRec.Code)
	}
}

// TestMaintenanceHandlers_Delete_RequiresCapability: a non-admin without
// can_manage_maintenance is READ-ONLY. The load-bearing assertion is that the
// window is still there afterwards — a 403 that deleted the row anyway would be
// the worst of both worlds.
func TestMaintenanceHandlers_Delete_RequiresCapability(t *testing.T) {
	h := newMaintenanceHarness(t)

	createRec := h.do(t, http.MethodPost, "/api/maintenance", h.tokenA, map[string]any{
		"title": "A-ToKeep", "strategy": "single", "monitor_ids": []int64{h.monitorBID},
	})
	var created map[string]any
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	id := floatToIntStr(created["id"].(float64))

	// User C can SEE this window (it covers monitor B) but must not be able to
	// delete it.
	otherRec := h.do(t, http.MethodDelete, "/api/maintenance/"+id, h.tokenC, nil)
	if otherRec.Code != http.StatusForbidden {
		t.Errorf("read-only user DELETE /api/maintenance/%s returned %d; want 403", id, otherRec.Code)
	}
	getRec := h.do(t, http.MethodGet, "/api/maintenance/"+id, h.tokenA, nil)
	if getRec.Code != http.StatusOK {
		t.Errorf("window was deleted by a user without the capability; GET returned %d", getRec.Code)
	}

	// The capability holder can delete it, even though the admin created it.
	ownRec := h.do(t, http.MethodDelete, "/api/maintenance/"+id, h.tokenB, nil)
	if ownRec.Code != http.StatusNoContent {
		t.Errorf("capability holder DELETE /api/maintenance/%s returned %d; want 204", id, ownRec.Code)
	}
}

// --- Monitor assignment (alert suppression) --------------------------------
//
// These guard the monitor-to-window links that alert suppression depends on.
// The load-bearing assertion in each test is IsActive() — the single thing the
// scheduler and the notification dispatcher actually consult. A 201 proves
// nothing.

func TestMaintenanceHandlers_Create_LinksMonitorsSoAlertsAreSuppressed(t *testing.T) {
	h := newMaintenanceHarness(t)
	ctx := context.Background()

	// Sanity: with no window, the monitor is not under maintenance.
	if active, err := h.maintSvc.IsActive(ctx, h.monitorAID); err != nil || active {
		t.Fatalf("IsActive before any window = (%v, %v); want (false, nil)", active, err)
	}

	// A window that is active right now, covering monitor A.
	now := time.Now().UTC()
	body := map[string]any{
		"title":       "Now",
		"strategy":    "single",
		"start_date":  now.Add(-time.Hour).Format(time.RFC3339),
		"end_date":    now.Add(time.Hour).Format(time.RFC3339),
		"monitor_ids": []int64{h.monitorAID},
	}
	rec := h.do(t, http.MethodPost, "/api/maintenance", h.tokenA, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/maintenance returned %d; want 201 (body: %s)", rec.Code, rec.Body.String())
	}

	// The link must be persisted, not just echoed back.
	active, err := h.maintSvc.IsActive(ctx, h.monitorAID)
	if err != nil {
		t.Fatalf("IsActive: %v", err)
	}
	if !active {
		t.Fatal("IsActive(monitor A) = false during an active window covering it; " +
			"the monitor_ids were dropped, so no alert would ever be suppressed")
	}

	// And it must not bleed onto monitors the window doesn't cover.
	if active, err := h.maintSvc.IsActive(ctx, h.monitorBID); err != nil || active {
		t.Fatalf("IsActive(monitor B, not covered) = (%v, %v); want (false, nil)", active, err)
	}

	// The response must carry monitor_ids, or the edit form reopens with an empty
	// selection and the next save silently unlinks every monitor.
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	ids, ok := created["monitor_ids"].([]any)
	if !ok || len(ids) != 1 || int64(ids[0].(float64)) != h.monitorAID {
		t.Fatalf("monitor_ids = %v; want [%d]", created["monitor_ids"], h.monitorAID)
	}
}

// A window only suppresses alerts while it is actually open — a link alone is not
// enough. Guards against a fix that makes IsActive true whenever a link exists.
func TestMaintenanceHandlers_Create_PastWindowDoesNotSuppress(t *testing.T) {
	h := newMaintenanceHarness(t)

	now := time.Now().UTC()
	body := map[string]any{
		"title":       "Yesterday",
		"strategy":    "single",
		"start_date":  now.Add(-48 * time.Hour).Format(time.RFC3339),
		"end_date":    now.Add(-24 * time.Hour).Format(time.RFC3339),
		"monitor_ids": []int64{h.monitorAID},
	}
	if rec := h.do(t, http.MethodPost, "/api/maintenance", h.tokenA, body); rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/maintenance returned %d; want 201", rec.Code)
	}

	if active, err := h.maintSvc.IsActive(context.Background(), h.monitorAID); err != nil || active {
		t.Fatalf("IsActive during a window that closed yesterday = (%v, %v); want (false, nil)", active, err)
	}
}

// Pointing a window at a monitor you cannot SEE would let you suppress its alerts.
// The capability to manage maintenance does not widen which monitors you can
// touch — that is what the grant is for, and this is the test that says so.
func TestMaintenanceHandlers_Create_RejectsMonitorTheCallerCannotSee(t *testing.T) {
	h := newMaintenanceHarness(t)

	now := time.Now().UTC()
	body := map[string]any{
		"title":       "Silence monitor A",
		"strategy":    "single",
		"start_date":  now.Add(-time.Hour).Format(time.RFC3339),
		"end_date":    now.Add(time.Hour).Format(time.RFC3339),
		"monitor_ids": []int64{h.monitorAID}, // user B was never granted monitor A
	}
	rec := h.do(t, http.MethodPost, "/api/maintenance", h.tokenB, body)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST with a monitor the caller cannot see returned %d; want 404", rec.Code)
	}

	// The real assertion: monitor A must NOT be suppressed.
	if active, err := h.maintSvc.IsActive(context.Background(), h.monitorAID); err != nil || active {
		t.Fatalf("IsActive(monitor A) = (%v, %v) after a rejected request from a user who cannot see it; "+
			"want (false, nil) — a rejected request must never silence a monitor", active, err)
	}
}

// TestMaintenanceHandlers_Create_RequiresCapability: a non-admin without the
// capability cannot create a window at all, and nothing is written.
func TestMaintenanceHandlers_Create_RequiresCapability(t *testing.T) {
	h := newMaintenanceHarness(t)

	now := time.Now().UTC()
	rec := h.do(t, http.MethodPost, "/api/maintenance", h.tokenC, map[string]any{
		"title":       "should not exist",
		"strategy":    "single",
		"start_date":  now.Add(-time.Hour).Format(time.RFC3339),
		"end_date":    now.Add(time.Hour).Format(time.RFC3339),
		"monitor_ids": []int64{h.monitorBID}, // a monitor C CAN see — capability is what's missing
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /api/maintenance without the capability returned %d; want 403", rec.Code)
	}

	// Effect, not status: monitor B must not have been put into maintenance.
	if active, err := h.maintSvc.IsActive(context.Background(), h.monitorBID); err != nil || active {
		t.Fatalf("IsActive(monitor B) = (%v, %v) after a 403'd create; want (false, nil)", active, err)
	}
}

func TestMaintenanceHandlers_Update_ReplacesMonitorSet(t *testing.T) {
	h := newMaintenanceHarness(t)
	ctx := context.Background()

	now := time.Now().UTC()
	start := now.Add(-time.Hour).Format(time.RFC3339)
	end := now.Add(time.Hour).Format(time.RFC3339)

	rec := h.do(t, http.MethodPost, "/api/maintenance", h.tokenA, map[string]any{
		"title": "W", "strategy": "single",
		"start_date": start, "end_date": end,
		"monitor_ids": []int64{h.monitorAID},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create returned %d; want 201", rec.Code)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	id := int64(created["id"].(float64))

	if active, _ := h.maintSvc.IsActive(ctx, h.monitorAID); !active {
		t.Fatal("monitor A should be suppressed after create")
	}

	// Drop the monitor from the window.
	rec = h.do(t, http.MethodPut, "/api/maintenance/"+strconv.FormatInt(id, 10), h.tokenA, map[string]any{
		"title": "W", "strategy": "single", "active": true,
		"start_date": start, "end_date": end,
		"monitor_ids": []int64{},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update returned %d; want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	if active, err := h.maintSvc.IsActive(ctx, h.monitorAID); err != nil || active {
		t.Fatalf("IsActive after removing the monitor from the window = (%v, %v); want (false, nil)", active, err)
	}
}

func TestMaintenanceHandlers_AssignUnassignMonitor_RoundTrip(t *testing.T) {
	h := newMaintenanceHarness(t)
	ctx := context.Background()

	now := time.Now().UTC()
	rec := h.do(t, http.MethodPost, "/api/maintenance", h.tokenA, map[string]any{
		"title": "W", "strategy": "single",
		"start_date": now.Add(-time.Hour).Format(time.RFC3339),
		"end_date":   now.Add(time.Hour).Format(time.RFC3339),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create returned %d; want 201", rec.Code)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	base := "/api/maintenance/" + strconv.FormatInt(int64(created["id"].(float64)), 10) + "/monitors"

	// Assign.
	if rec := h.do(t, http.MethodPost, base, h.tokenA, map[string]any{"monitor_id": h.monitorAID}); rec.Code != http.StatusNoContent {
		t.Fatalf("assign returned %d; want 204 (body: %s)", rec.Code, rec.Body.String())
	}
	if active, _ := h.maintSvc.IsActive(ctx, h.monitorAID); !active {
		t.Fatal("IsActive = false after POST /monitors; the assign endpoint did not persist a link")
	}

	// ListMonitors must reflect it.
	rec = h.do(t, http.MethodGet, base, h.tokenA, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list monitors returned %d; want 200", rec.Code)
	}
	var ids []int64
	if err := json.Unmarshal(rec.Body.Bytes(), &ids); err != nil {
		t.Fatalf("unmarshal ids: %v", err)
	}
	if len(ids) != 1 || ids[0] != h.monitorAID {
		t.Fatalf("GET /monitors = %v; want [%d]", ids, h.monitorAID)
	}

	// Unassign.
	if rec := h.do(t, http.MethodDelete, base+"/"+strconv.FormatInt(h.monitorAID, 10), h.tokenA, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("unassign returned %d; want 204", rec.Code)
	}
	if active, err := h.maintSvc.IsActive(ctx, h.monitorAID); err != nil || active {
		t.Fatalf("IsActive after unassign = (%v, %v); want (false, nil)", active, err)
	}
}
