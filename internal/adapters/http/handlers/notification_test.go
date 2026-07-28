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
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// --- Fakes ----------------------------------------------------------------
//
// NotificationRepository.GetByMonitorID is what ListForMonitors (and therefore
// the non-manager List path) consults. The monitor-link store must therefore
// actually feed GetByMonitorID — a stub that always returns [] would make every
// scoping test pass while the product rule never ran.

type notifHMonLinkRepo struct {
	mu     sync.Mutex
	nextID int64
	links  []domain.MonitorNotification
}

func newNotifHMonLinkRepo() *notifHMonLinkRepo {
	return &notifHMonLinkRepo{}
}

func (r *notifHMonLinkRepo) Attach(_ context.Context, monitorID, notificationID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.links {
		if l.MonitorID == monitorID && l.NotificationID == notificationID {
			return nil // idempotent
		}
	}
	r.nextID++
	r.links = append(r.links, domain.MonitorNotification{
		ID: r.nextID, MonitorID: monitorID, NotificationID: notificationID,
	})
	return nil
}

func (r *notifHMonLinkRepo) Detach(_ context.Context, monitorID, notificationID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.links[:0]
	for _, l := range r.links {
		if l.MonitorID == monitorID && l.NotificationID == notificationID {
			continue
		}
		out = append(out, l)
	}
	r.links = out
	return nil
}

func (r *notifHMonLinkRepo) ListByMonitor(_ context.Context, monitorID int64) ([]*domain.MonitorNotification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.MonitorNotification, 0)
	for i := range r.links {
		if r.links[i].MonitorID == monitorID {
			cp := r.links[i]
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *notifHMonLinkRepo) ListByNotification(_ context.Context, notificationID int64) ([]*domain.MonitorNotification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.MonitorNotification, 0)
	for i := range r.links {
		if r.links[i].NotificationID == notificationID {
			cp := r.links[i]
			out = append(out, &cp)
		}
	}
	return out, nil
}

// hasLink is a test-only effect probe (bypass HTTP).
func (r *notifHMonLinkRepo) hasLink(monitorID, notificationID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.links {
		if l.MonitorID == monitorID && l.NotificationID == notificationID {
			return true
		}
	}
	return false
}

type notifHRepo struct {
	mu       sync.Mutex
	byID     map[int64]*domain.Notification
	nextID   int64
	monLinks *notifHMonLinkRepo
}

func newNotifHRepo(monLinks *notifHMonLinkRepo) *notifHRepo {
	return &notifHRepo{byID: make(map[int64]*domain.Notification), monLinks: monLinks}
}

func cloneNotifH(n *domain.Notification) *domain.Notification {
	if n == nil {
		return nil
	}
	cp := *n
	if n.Config != nil {
		cp.Config = make(map[string]any, len(n.Config))
		for k, v := range n.Config {
			cp.Config[k] = v
		}
	}
	return &cp
}

func (r *notifHRepo) Create(_ context.Context, n *domain.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	n.ID = r.nextID
	now := time.Now().UTC()
	if n.CreatedAt.IsZero() {
		n.CreatedAt = now
	}
	if n.UpdatedAt.IsZero() {
		n.UpdatedAt = now
	}
	r.byID[n.ID] = cloneNotifH(n)
	return nil
}

func (r *notifHRepo) GetByID(_ context.Context, id int64) (*domain.Notification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return cloneNotifH(n), nil
}

// GetByMonitorID joins through monLinks so ListForMonitors scopes correctly.
func (r *notifHRepo) GetByMonitorID(ctx context.Context, monitorID int64) ([]*domain.Notification, error) {
	links, err := r.monLinks.ListByMonitor(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Notification, 0, len(links))
	for _, l := range links {
		n, err := r.GetByID(ctx, l.NotificationID)
		if err != nil {
			continue // deleted notification, skip dangling link
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *notifHRepo) List(_ context.Context, userID int64) ([]*domain.Notification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Notification, 0)
	for _, n := range r.byID {
		if userID > 0 && n.UserID != userID {
			continue
		}
		out = append(out, cloneNotifH(n))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *notifHRepo) ListAll(ctx context.Context) ([]*domain.Notification, error) {
	return r.List(ctx, 0)
}

func (r *notifHRepo) Update(_ context.Context, n *domain.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[n.ID]; !ok {
		return ports.ErrNotFound
	}
	n.UpdatedAt = time.Now().UTC()
	r.byID[n.ID] = cloneNotifH(n)
	return nil
}

func (r *notifHRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[id]; !ok {
		return ports.ErrNotFound
	}
	delete(r.byID, id)
	return nil
}

// count is a test-only effect probe.
func (r *notifHRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

type notifHGroupLinkRepo struct {
	mu     sync.Mutex
	nextID int64
	links  []domain.GroupNotification
	notif  *notifHRepo
}

func newNotifHGroupLinkRepo(notif *notifHRepo) *notifHGroupLinkRepo {
	return &notifHGroupLinkRepo{notif: notif}
}

func (r *notifHGroupLinkRepo) Attach(_ context.Context, groupID, notificationID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.links {
		if l.GroupID == groupID && l.NotificationID == notificationID {
			return nil // idempotent per port contract
		}
	}
	r.nextID++
	r.links = append(r.links, domain.GroupNotification{
		ID: r.nextID, GroupID: groupID, NotificationID: notificationID,
	})
	return nil
}

func (r *notifHGroupLinkRepo) Detach(_ context.Context, groupID, notificationID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.links[:0]
	for _, l := range r.links {
		if l.GroupID == groupID && l.NotificationID == notificationID {
			continue
		}
		out = append(out, l)
	}
	r.links = out
	return nil
}

func (r *notifHGroupLinkRepo) ListByGroup(_ context.Context, groupID int64) ([]*domain.GroupNotification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.GroupNotification, 0)
	for i := range r.links {
		if r.links[i].GroupID == groupID {
			cp := r.links[i]
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *notifHGroupLinkRepo) ListByNotification(_ context.Context, notificationID int64) ([]*domain.GroupNotification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.GroupNotification, 0)
	for i := range r.links {
		if r.links[i].NotificationID == notificationID {
			cp := r.links[i]
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *notifHGroupLinkRepo) ListNotificationsByGroup(ctx context.Context, groupID int64) ([]*domain.Notification, error) {
	links, err := r.ListByGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Notification, 0, len(links))
	for _, l := range links {
		n, err := r.notif.GetByID(ctx, l.NotificationID)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *notifHGroupLinkRepo) hasLink(groupID, notificationID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.links {
		if l.GroupID == groupID && l.NotificationID == notificationID {
			return true
		}
	}
	return false
}

// notifHSender records every Send call so Test can assert a real dispatch
// attempt, not just a 200.
type notifHSender struct {
	mu    sync.Mutex
	sends []notifHSendCall
}

type notifHSendCall struct {
	Config map[string]any
	Alert  domain.AlertContext
}

func (s *notifHSender) Type() string { return "webhook" }

func (s *notifHSender) Validate(_ map[string]any) error { return nil }

func (s *notifHSender) Send(_ context.Context, config map[string]any, alert domain.AlertContext) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make(map[string]any, len(config))
	for k, v := range config {
		cp[k] = v
	}
	s.sends = append(s.sends, notifHSendCall{Config: cp, Alert: alert})
	return nil
}

func (s *notifHSender) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sends)
}

func (s *notifHSender) last() (notifHSendCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sends) == 0 {
		return notifHSendCall{}, false
	}
	return s.sends[len(s.sends)-1], true
}

// --- Harness --------------------------------------------------------------
//
// Three principals mirror the maintenance / notification RBAC product rules:
//
//	A — admin. Sees every monitor/group/notification; may do anything.
//	B — non-admin holding can_manage_notifications, granted monitor B + group B.
//	    May create/edit/delete ANY notification (install-wide), but may only
//	    attach to monitors/groups they can view.
//	C — non-admin with NO capability, granted monitor B + group B only.
//	    Read-only, and only for notifications attached to monitors they can see.

type notifHTTPHarness struct {
	router *echo.Echo

	tokenA  string // admin
	tokenB  string // can_manage_notifications
	tokenC  string // no capability
	userBID int64

	svc        *services.NotificationService
	repo       *notifHRepo
	monLinks   *notifHMonLinkRepo
	groupLinks *notifHGroupLinkRepo
	sender     *notifHSender

	monitorAID int64
	monitorBID int64
	groupAID   int64
	groupBID   int64
}

func newNotifHTTPHarness(t *testing.T) *notifHTTPHarness {
	t.Helper()
	ctx := context.Background()

	userRepo := memory.NewUserRepo()
	apiKeyRepo := memory.NewAPIKeyRepo()
	permRepo := memory.NewUserPermissionRepo()
	authenticator := auth.NewJWTAuthenticator("notif-handler-test-key", 24, userRepo)
	authSvc := services.NewAuthService(
		userRepo, apiKeyRepo, authenticator, auth.NewTOTPProvider("Phoenix"),
	)

	// User A: first user via Register — always admin.
	if _, err := authSvc.Register(ctx, "notif-user-a", "password123"); err != nil {
		t.Fatalf("register user A: %v", err)
	}
	tokenA, err := authSvc.Login(ctx, "notif-user-a", "password123")
	if err != nil {
		t.Fatalf("login user A: %v", err)
	}

	// User B: capability holder.
	userB, err := authSvc.CreateUser(ctx, "notif-user-b", "password123", true, false, "UTC",
		services.UserCapabilities{CanManageNotifications: true})
	if err != nil {
		t.Fatalf("create user B: %v", err)
	}
	tokenB, err := authSvc.Login(ctx, "notif-user-b", "password123")
	if err != nil {
		t.Fatalf("login user B: %v", err)
	}

	// User C: read-only, no capabilities.
	userC, err := authSvc.CreateUser(ctx, "notif-user-c", "password123", true, false, "UTC",
		services.UserCapabilities{})
	if err != nil {
		t.Fatalf("create user C: %v", err)
	}
	tokenC, err := authSvc.Login(ctx, "notif-user-c", "password123")
	if err != nil {
		t.Fatalf("login user C: %v", err)
	}

	monitorRepo := newFakeMonitorRepo()
	groupRepo := newFakeMonitorGroupRepo()

	// Two monitors + two folders. Ownership is irrelevant under RBAC; grants are.
	monitorA := &domain.Monitor{UserID: 1, Name: "a-monitor", Type: "http", Active: true, Interval: 60}
	monitorB := &domain.Monitor{UserID: 1, Name: "b-monitor", Type: "http", Active: true, Interval: 60}
	if err := monitorRepo.Create(ctx, monitorA); err != nil {
		t.Fatalf("create monitor A: %v", err)
	}
	if err := monitorRepo.Create(ctx, monitorB); err != nil {
		t.Fatalf("create monitor B: %v", err)
	}

	groupA := &domain.MonitorGroup{UserID: 1, Name: "a-group", Condition: domain.GroupConditionWorstOfChildren}
	groupB := &domain.MonitorGroup{UserID: 1, Name: "b-group", Condition: domain.GroupConditionWorstOfChildren}
	if err := groupRepo.Create(ctx, groupA); err != nil {
		t.Fatalf("create group A: %v", err)
	}
	if err := groupRepo.Create(ctx, groupB); err != nil {
		t.Fatalf("create group B: %v", err)
	}

	// B and C can see monitor B and group B only.
	if err := permRepo.Grant(ctx, &domain.UserPermission{UserID: userB.ID, MonitorID: &monitorB.ID}); err != nil {
		t.Fatalf("grant monitor B to user B: %v", err)
	}
	if err := permRepo.Grant(ctx, &domain.UserPermission{
		UserID: userB.ID, GroupID: &groupB.ID, IncludeDescendants: true,
	}); err != nil {
		t.Fatalf("grant group B to user B: %v", err)
	}
	if err := permRepo.Grant(ctx, &domain.UserPermission{UserID: userC.ID, MonitorID: &monitorB.ID}); err != nil {
		t.Fatalf("grant monitor B to user C: %v", err)
	}
	if err := permRepo.Grant(ctx, &domain.UserPermission{
		UserID: userC.ID, GroupID: &groupB.ID, IncludeDescendants: true,
	}); err != nil {
		t.Fatalf("grant group B to user C: %v", err)
	}

	monLinks := newNotifHMonLinkRepo()
	repo := newNotifHRepo(monLinks)
	groupLinks := newNotifHGroupLinkRepo(repo)
	sender := &notifHSender{}

	notifSvc := services.NewNotificationService(repo, monLinks)
	notifSvc.SetGroupNotificationRepo(groupLinks)
	notifSvc.RegisterSender(sender)

	accessSvc := services.NewAccessService(userRepo, permRepo, groupRepo, monitorRepo)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	// Wiring MUST mirror internal/adapters/http/router.go so middleware gates
	// are under test, not only the in-handler requireManageNotifications checks.
	requireNotifications := middleware.RequireCapability(accessSvc, middleware.CapManageNotifications)
	nh := handlers.NewNotificationHandlers(notifSvc, accessSvc)

	notifGroup := e.Group("/api/notifications", middleware.AuthMiddleware(authSvc))
	notifGroup.POST("", nh.Create, requireNotifications)
	notifGroup.GET("", nh.List)
	notifGroup.GET("/:id", nh.GetByID)
	notifGroup.PUT("/:id", nh.Update, requireNotifications)
	notifGroup.DELETE("/:id", nh.Delete, requireNotifications)
	notifGroup.POST("/:id/test", nh.Test, requireNotifications)
	notifGroup.POST("/:id/monitor/:monitorId", nh.AttachToMonitor, requireNotifications)
	notifGroup.DELETE("/:id/monitor/:monitorId", nh.DetachFromMonitor, requireNotifications)
	notifGroup.POST("/:id/group/:groupId", nh.AttachToGroup, requireNotifications)
	notifGroup.DELETE("/:id/group/:groupId", nh.DetachFromGroup, requireNotifications)

	monNotifGroup := e.Group("/api/monitors/:id/notifications", middleware.AuthMiddleware(authSvc))
	monNotifGroup.GET("", nh.ListForMonitor)

	groupNotifGroup := e.Group("/api/monitor-groups/:id/notifications", middleware.AuthMiddleware(authSvc))
	groupNotifGroup.GET("", nh.ListForGroup)

	return &notifHTTPHarness{
		router:     e,
		tokenA:     tokenA,
		tokenB:     tokenB,
		tokenC:     tokenC,
		userBID:    userB.ID,
		svc:        notifSvc,
		repo:       repo,
		monLinks:   monLinks,
		groupLinks: groupLinks,
		sender:     sender,
		monitorAID: monitorA.ID,
		monitorBID: monitorB.ID,
		groupAID:   groupA.ID,
		groupBID:   groupB.ID,
	}
}

func (h *notifHTTPHarness) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
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

// createNotif is a helper for seed data: capability holder (or admin) creates a
// webhook notification and returns its wire id.
func (h *notifHTTPHarness) createNotif(t *testing.T, token, name string) int64 {
	t.Helper()
	rec := h.do(t, http.MethodPost, "/api/notifications", token, map[string]any{
		"name":   name,
		"type":   "webhook",
		"config": map[string]any{"url": "https://hooks.example/" + name},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create %q: %d (%s)", name, rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create %q: %v", name, err)
	}
	id, ok := created["id"].(float64)
	if !ok || id <= 0 {
		t.Fatalf("create %q returned id=%v", name, created["id"])
	}
	return int64(id)
}

func decodeErrorMsg(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (raw: %s)", err, rec.Body.String())
	}
	msg, _ := body["error"].(string)
	return msg
}

// --- Tests ----------------------------------------------------------------

// TestNotificationHandlers_Create_CapabilityHolderSucceeds asserts Create
// persists a row with the expected fields (effect), and the wire shape is
// NotificationView (snake_case), not a raw domain.Notification.
func TestNotificationHandlers_Create_CapabilityHolderSucceeds(t *testing.T) {
	h := newNotifHTTPHarness(t)

	body := map[string]any{
		"name":       "Pager webhook",
		"type":       "webhook",
		"is_default": true,
		"config":     map[string]any{"url": "https://hooks.example/pager"},
	}
	rec := h.do(t, http.MethodPost, "/api/notifications", h.tokenB, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/notifications returned %d; want 201 (body: %s)", rec.Code, rec.Body.String())
	}

	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal view: %v", err)
	}
	// Wire shape checks — snake_case tags from NotificationView.
	if view["name"] != "Pager webhook" {
		t.Errorf("name = %v; want Pager webhook", view["name"])
	}
	if view["type"] != "webhook" {
		t.Errorf("type = %v; want webhook", view["type"])
	}
	if view["active"] != true {
		t.Errorf("active = %v; want true (default when omitted)", view["active"])
	}
	if view["is_default"] != true {
		t.Errorf("is_default = %v; want true", view["is_default"])
	}
	// user_id must come from the authenticated principal, not the body.
	if uid, ok := view["user_id"].(float64); !ok || int64(uid) != h.userBID {
		t.Errorf("user_id = %v; want %d (capability holder's id)", view["user_id"], h.userBID)
	}
	cfg, ok := view["config"].(map[string]any)
	if !ok || cfg["url"] != "https://hooks.example/pager" {
		t.Errorf("config = %v; want url=https://hooks.example/pager", view["config"])
	}

	// Effect: the row is in the store, not just echoed.
	id := int64(view["id"].(float64))
	stored, err := h.svc.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("svc.GetByID after create: %v", err)
	}
	if stored.Name != "Pager webhook" || stored.Type != "webhook" || !stored.IsDefault || !stored.Active {
		t.Errorf("stored = %+v; want name/type/is_default/active matching request", stored)
	}
	if stored.UserID != h.userBID {
		t.Errorf("stored.UserID = %d; want %d", stored.UserID, h.userBID)
	}
}

// TestNotificationHandlers_CreateUpdateDelete_DeniedForNonCapability asserts a
// non-admin without can_manage_notifications gets 403 on mutations AND that
// nothing is written / nothing is changed / nothing is deleted.
func TestNotificationHandlers_CreateUpdateDelete_DeniedForNonCapability(t *testing.T) {
	h := newNotifHTTPHarness(t)
	ctx := context.Background()

	// Seed one notification as the capability holder so Update/Delete have a target.
	id := h.createNotif(t, h.tokenB, "keep-me")
	beforeCount := h.repo.count()

	// --- Create denied ----------------------------------------------------
	createRec := h.do(t, http.MethodPost, "/api/notifications", h.tokenC, map[string]any{
		"name": "should-not-exist", "type": "webhook",
		"config": map[string]any{"url": "https://hooks.example/nope"},
	})
	if createRec.Code != http.StatusForbidden {
		t.Fatalf("Create without capability returned %d; want 403", createRec.Code)
	}
	if h.repo.count() != beforeCount {
		t.Errorf("Create 403 still wrote a row: count %d → %d", beforeCount, h.repo.count())
	}

	// --- Update denied ----------------------------------------------------
	updateRec := h.do(t, http.MethodPut, "/api/notifications/"+floatToIntStr(float64(id)), h.tokenC, map[string]any{
		"name": "renamed-by-intruder",
	})
	if updateRec.Code != http.StatusForbidden {
		t.Fatalf("Update without capability returned %d; want 403", updateRec.Code)
	}
	stored, err := h.svc.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID after denied update: %v", err)
	}
	if stored.Name != "keep-me" {
		t.Errorf("name was mutated by a 403'd Update: got %q", stored.Name)
	}

	// --- Delete denied ----------------------------------------------------
	deleteRec := h.do(t, http.MethodDelete, "/api/notifications/"+floatToIntStr(float64(id)), h.tokenC, nil)
	if deleteRec.Code != http.StatusForbidden {
		t.Fatalf("Delete without capability returned %d; want 403", deleteRec.Code)
	}
	if _, err := h.svc.GetByID(ctx, id); err != nil {
		t.Errorf("row was deleted by a 403'd Delete: %v", err)
	}
}

// TestNotificationHandlers_List_ScopesForNonManager verifies the product rule
// from NotificationHandlers.List: capability holders see every notification;
// everyone else sees only those attached to monitors they can view.
func TestNotificationHandlers_List_ScopesForNonManager(t *testing.T) {
	h := newNotifHTTPHarness(t)

	hiddenID := h.createNotif(t, h.tokenA, "hidden-on-A")
	visibleID := h.createNotif(t, h.tokenA, "visible-on-B")
	orphanID := h.createNotif(t, h.tokenA, "orphan-unattached")

	// Attach hidden → monitor A (invisible to C); visible → monitor B.
	if rec := h.do(t, http.MethodPost,
		"/api/notifications/"+floatToIntStr(float64(hiddenID))+"/monitor/"+floatToIntStr(float64(h.monitorAID)),
		h.tokenA, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("attach hidden→A: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := h.do(t, http.MethodPost,
		"/api/notifications/"+floatToIntStr(float64(visibleID))+"/monitor/"+floatToIntStr(float64(h.monitorBID)),
		h.tokenA, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("attach visible→B: %d (%s)", rec.Code, rec.Body.String())
	}

	// Capability holder (B) sees the FULL install list — all three.
	listB := h.do(t, http.MethodGet, "/api/notifications", h.tokenB, nil)
	if listB.Code != http.StatusOK {
		t.Fatalf("capability holder List = %d; want 200", listB.Code)
	}
	var all []map[string]any
	if err := json.Unmarshal(listB.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode list B: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("capability holder sees %d notifications; want 3 (install-wide)", len(all))
	}

	// Non-manager (C) sees ONLY the one attached to a visible monitor.
	listC := h.do(t, http.MethodGet, "/api/notifications", h.tokenC, nil)
	if listC.Code != http.StatusOK {
		t.Fatalf("non-manager List = %d; want 200", listC.Code)
	}
	var scoped []map[string]any
	if err := json.Unmarshal(listC.Body.Bytes(), &scoped); err != nil {
		t.Fatalf("decode list C: %v", err)
	}
	if len(scoped) != 1 {
		t.Fatalf("non-manager sees %d notifications; want 1 (only attached to visible monitor B)", len(scoped))
	}
	if int64(scoped[0]["id"].(float64)) != visibleID {
		t.Errorf("non-manager saw id=%v; want %d (leaked hidden/orphan)", scoped[0]["id"], visibleID)
	}
	// Name check — content, not just length.
	if scoped[0]["name"] != "visible-on-B" {
		t.Errorf("non-manager list name = %v; want visible-on-B", scoped[0]["name"])
	}

	// Orphan (unattached) must never appear for a non-manager.
	for _, n := range scoped {
		if int64(n["id"].(float64)) == orphanID {
			t.Error("non-manager list leaked the unattached orphan notification")
		}
		if int64(n["id"].(float64)) == hiddenID {
			t.Error("non-manager list leaked the notification attached to an invisible monitor")
		}
	}
}

// TestNotificationHandlers_AttachDetachMonitor_Effect asserts the link is
// actually written on attach and gone on detach (AGENTS rule 7 — no stub 204).
func TestNotificationHandlers_AttachDetachMonitor_Effect(t *testing.T) {
	h := newNotifHTTPHarness(t)
	ctx := context.Background()

	id := h.createNotif(t, h.tokenB, "mon-link")
	path := "/api/notifications/" + floatToIntStr(float64(id)) +
		"/monitor/" + floatToIntStr(float64(h.monitorBID))

	// Attach.
	attach := h.do(t, http.MethodPost, path, h.tokenB, nil)
	if attach.Code != http.StatusNoContent {
		t.Fatalf("AttachToMonitor returned %d; want 204 (%s)", attach.Code, attach.Body.String())
	}
	if !h.monLinks.hasLink(h.monitorBID, id) {
		t.Fatal("Attach returned 204 but monLinks has no row — silent stub")
	}
	// Effect via the read path used by the monitor detail page.
	listRec := h.do(t, http.MethodGet,
		"/api/monitors/"+floatToIntStr(float64(h.monitorBID))+"/notifications", h.tokenB, nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("ListForMonitor = %d; want 200", listRec.Code)
	}
	var monNotifs []map[string]any
	if err := json.Unmarshal(listRec.Body.Bytes(), &monNotifs); err != nil {
		t.Fatalf("decode ListForMonitor: %v", err)
	}
	if len(monNotifs) != 1 || int64(monNotifs[0]["id"].(float64)) != id {
		t.Fatalf("ListForMonitor after attach = %v; want [{id:%d}]", monNotifs, id)
	}

	// Detach.
	detach := h.do(t, http.MethodDelete, path, h.tokenB, nil)
	if detach.Code != http.StatusNoContent {
		t.Fatalf("DetachFromMonitor returned %d; want 204 (%s)", detach.Code, detach.Body.String())
	}
	if h.monLinks.hasLink(h.monitorBID, id) {
		t.Fatal("Detach returned 204 but monLinks still has the row")
	}
	// Service path agrees.
	links, err := h.svc.ListByNotification(ctx, id)
	if err != nil {
		t.Fatalf("ListByNotification after detach: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("ListByNotification after detach = %v; want empty", links)
	}
}

// TestNotificationHandlers_AttachDetachGroup_Effect asserts group↔notification
// links are persisted (folder alerting attachment), not stubbed.
func TestNotificationHandlers_AttachDetachGroup_Effect(t *testing.T) {
	h := newNotifHTTPHarness(t)
	ctx := context.Background()

	id := h.createNotif(t, h.tokenB, "group-link")
	path := "/api/notifications/" + floatToIntStr(float64(id)) +
		"/group/" + floatToIntStr(float64(h.groupBID))

	attach := h.do(t, http.MethodPost, path, h.tokenB, nil)
	if attach.Code != http.StatusNoContent {
		t.Fatalf("AttachToGroup returned %d; want 204 (%s)", attach.Code, attach.Body.String())
	}
	if !h.groupLinks.hasLink(h.groupBID, id) {
		t.Fatal("AttachToGroup returned 204 but groupLinks has no row — silent stub")
	}
	// Effect via ListForGroup (the folder form's checkbox list).
	listRec := h.do(t, http.MethodGet,
		"/api/monitor-groups/"+floatToIntStr(float64(h.groupBID))+"/notifications", h.tokenB, nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("ListForGroup = %d; want 200", listRec.Code)
	}
	var groupNotifs []map[string]any
	if err := json.Unmarshal(listRec.Body.Bytes(), &groupNotifs); err != nil {
		t.Fatalf("decode ListForGroup: %v", err)
	}
	if len(groupNotifs) != 1 || int64(groupNotifs[0]["id"].(float64)) != id {
		t.Fatalf("ListForGroup after attach = %v; want [{id:%d}]", groupNotifs, id)
	}
	// Service agrees.
	svcList, err := h.svc.GetByGroupID(ctx, h.groupBID)
	if err != nil {
		t.Fatalf("GetByGroupID: %v", err)
	}
	if len(svcList) != 1 || svcList[0].ID != id {
		t.Fatalf("svc.GetByGroupID = %v; want [{id:%d}]", svcList, id)
	}

	detach := h.do(t, http.MethodDelete, path, h.tokenB, nil)
	if detach.Code != http.StatusNoContent {
		t.Fatalf("DetachFromGroup returned %d; want 204 (%s)", detach.Code, detach.Body.String())
	}
	if h.groupLinks.hasLink(h.groupBID, id) {
		t.Fatal("DetachFromGroup returned 204 but groupLinks still has the row")
	}
	svcList, err = h.svc.GetByGroupID(ctx, h.groupBID)
	if err != nil {
		t.Fatalf("GetByGroupID after detach: %v", err)
	}
	if len(svcList) != 0 {
		t.Errorf("GetByGroupID after detach = %v; want empty", svcList)
	}
}

// TestNotificationHandlers_Test_InvokesSender asserts POST /:id/test really
// dispatches through the registered sender (a 200 that never called Send is the
// exact dead-feature shape AGENTS rule 7 forbids).
func TestNotificationHandlers_Test_InvokesSender(t *testing.T) {
	h := newNotifHTTPHarness(t)

	id := h.createNotif(t, h.tokenB, "test-me")
	if h.sender.callCount() != 0 {
		t.Fatalf("sender already has %d calls before Test", h.sender.callCount())
	}

	rec := h.do(t, http.MethodPost,
		"/api/notifications/"+floatToIntStr(float64(id))+"/test", h.tokenB, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /:id/test returned %d; want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode test response: %v", err)
	}
	if body["status"] != "sent" {
		t.Errorf("status = %v; want sent", body["status"])
	}

	if h.sender.callCount() != 1 {
		t.Fatalf("sender call count = %d; want 1 (Test must invoke Send)", h.sender.callCount())
	}
	call, ok := h.sender.last()
	if !ok {
		t.Fatal("no send call recorded")
	}
	if call.Config["url"] != "https://hooks.example/test-me" {
		t.Errorf("send config url = %v; want the notification's config", call.Config["url"])
	}
	if call.Alert.Message == "" {
		t.Error("send alert Message is empty; SendTest should set a test message")
	}
	if call.Alert.MonitorName != "Test Monitor" {
		t.Errorf("MonitorName = %q; want Test Monitor (SendTest fixture)", call.Alert.MonitorName)
	}

	// Denial: non-capability user must not fire a real send either.
	before := h.sender.callCount()
	denied := h.do(t, http.MethodPost,
		"/api/notifications/"+floatToIntStr(float64(id))+"/test", h.tokenC, nil)
	if denied.Code != http.StatusForbidden {
		t.Errorf("Test without capability returned %d; want 403", denied.Code)
	}
	if h.sender.callCount() != before {
		t.Errorf("denied Test still invoked sender: count %d → %d", before, h.sender.callCount())
	}
}

// TestNotificationHandlers_Get_UnknownOrInvisibleReturns404 asserts the shared
// not-found message for both missing ids and notifications the caller may not
// see (never 403 — never confirm existence).
func TestNotificationHandlers_Get_UnknownOrInvisibleReturns404(t *testing.T) {
	h := newNotifHTTPHarness(t)

	hiddenID := h.createNotif(t, h.tokenA, "private-on-A")
	// Attach only to monitor A so C cannot see it via any visible monitor.
	if rec := h.do(t, http.MethodPost,
		"/api/notifications/"+floatToIntStr(float64(hiddenID))+"/monitor/"+floatToIntStr(float64(h.monitorAID)),
		h.tokenA, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("attach hidden: %d (%s)", rec.Code, rec.Body.String())
	}

	const wantMsg = "notification not found"

	// Unknown id.
	unknown := h.do(t, http.MethodGet, "/api/notifications/999999", h.tokenC, nil)
	if unknown.Code != http.StatusNotFound {
		t.Errorf("GET unknown id returned %d; want 404", unknown.Code)
	}
	if msg := decodeErrorMsg(t, unknown); msg != wantMsg {
		t.Errorf("unknown id error = %q; want %q", msg, wantMsg)
	}

	// Exists but invisible to non-manager C → same 404, same message.
	invisible := h.do(t, http.MethodGet,
		"/api/notifications/"+floatToIntStr(float64(hiddenID)), h.tokenC, nil)
	if invisible.Code != http.StatusNotFound {
		t.Errorf("GET invisible id returned %d; want 404 (must not confirm existence)", invisible.Code)
	}
	if msg := decodeErrorMsg(t, invisible); msg != wantMsg {
		t.Errorf("invisible id error = %q; want %q", msg, wantMsg)
	}

	// Admin and capability holder can still fetch it (install-wide manage).
	for label, token := range map[string]string{"admin": h.tokenA, "manager": h.tokenB} {
		rec := h.do(t, http.MethodGet,
			"/api/notifications/"+floatToIntStr(float64(hiddenID)), token, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s GET hidden id returned %d; want 200", label, rec.Code)
		}
	}
}

// TestNotificationHandlers_Attach_RejectsMonitorCallerCannotSee: the manage
// capability does not widen which monitors you can wire a notification onto.
// A rejected attach must leave no link (effect).
func TestNotificationHandlers_Attach_RejectsMonitorCallerCannotSee(t *testing.T) {
	h := newNotifHTTPHarness(t)

	id := h.createNotif(t, h.tokenB, "no-touch-A")
	path := "/api/notifications/" + floatToIntStr(float64(id)) +
		"/monitor/" + floatToIntStr(float64(h.monitorAID))

	rec := h.do(t, http.MethodPost, path, h.tokenB, nil)
	// requireMonitorViewAccess returns 404 (not 403) when the monitor is out of scope.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("Attach to invisible monitor returned %d; want 404", rec.Code)
	}
	if h.monLinks.hasLink(h.monitorAID, id) {
		t.Fatal("rejected Attach still wrote a monitor link")
	}
}

// TestNotificationHandlers_Attach_RejectsGroupCallerCannotSee: same gate for
// folders — capability holder may not attach to a group they cannot view.
func TestNotificationHandlers_Attach_RejectsGroupCallerCannotSee(t *testing.T) {
	h := newNotifHTTPHarness(t)

	id := h.createNotif(t, h.tokenB, "no-touch-group-A")
	path := "/api/notifications/" + floatToIntStr(float64(id)) +
		"/group/" + floatToIntStr(float64(h.groupAID))

	rec := h.do(t, http.MethodPost, path, h.tokenB, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("Attach to invisible group returned %d; want 404", rec.Code)
	}
	if h.groupLinks.hasLink(h.groupAID, id) {
		t.Fatal("rejected Attach still wrote a group link")
	}
}

// TestNotificationHandlers_Update_CapabilityHolderMutatesAny asserts a
// capability holder can update a notification the admin created (ownership is
// not a gate), and the change is persisted.
func TestNotificationHandlers_Update_CapabilityHolderMutatesAny(t *testing.T) {
	h := newNotifHTTPHarness(t)
	ctx := context.Background()

	id := h.createNotif(t, h.tokenA, "admin-created")
	active := false
	rec := h.do(t, http.MethodPut, "/api/notifications/"+floatToIntStr(float64(id)), h.tokenB, map[string]any{
		"name":   "renamed-by-manager",
		"active": active,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("Update returned %d; want 200 (%s)", rec.Code, rec.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view["name"] != "renamed-by-manager" {
		t.Errorf("view name = %v; want renamed-by-manager", view["name"])
	}
	if view["active"] != false {
		t.Errorf("view active = %v; want false", view["active"])
	}

	stored, err := h.svc.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.Name != "renamed-by-manager" || stored.Active {
		t.Errorf("stored after update = name=%q active=%v; want renamed/inactive", stored.Name, stored.Active)
	}
}

// TestNotificationHandlers_Delete_CapabilityHolderRemovesRow asserts Delete
// actually removes the row for a capability holder.
func TestNotificationHandlers_Delete_CapabilityHolderRemovesRow(t *testing.T) {
	h := newNotifHTTPHarness(t)
	ctx := context.Background()

	id := h.createNotif(t, h.tokenA, "to-delete")
	rec := h.do(t, http.MethodDelete, "/api/notifications/"+floatToIntStr(float64(id)), h.tokenB, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("Delete returned %d; want 204 (%s)", rec.Code, rec.Body.String())
	}
	if _, err := h.svc.GetByID(ctx, id); err == nil {
		t.Fatal("row still present after Delete — 204 without effect")
	}
	// Double-check via the repo itself so a service that swallows not-found
	// cannot hide a failed delete.
	if _, err := h.repo.GetByID(ctx, id); err == nil {
		t.Fatal("row still present in repo after Delete")
	}
}
