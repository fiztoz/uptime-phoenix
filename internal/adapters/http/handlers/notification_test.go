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
	"strings"
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

type notifHTemplateRepo struct {
	mu     sync.Mutex
	nextID int64
	items  map[int64]*domain.NotificationTemplate
}

func newNotifHTemplateRepo() *notifHTemplateRepo {
	return &notifHTemplateRepo{items: make(map[int64]*domain.NotificationTemplate)}
}

func (r *notifHTemplateRepo) Create(_ context.Context, template *domain.NotificationTemplate) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	template.ID = r.nextID
	template.CreatedAt = time.Now().UTC()
	template.UpdatedAt = template.CreatedAt
	copy := *template
	r.items[template.ID] = &copy
	return nil
}

func (r *notifHTemplateRepo) GetByID(_ context.Context, id int64) (*domain.NotificationTemplate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	template, ok := r.items[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	copy := *template
	return &copy, nil
}

func (r *notifHTemplateRepo) List(_ context.Context) ([]*domain.NotificationTemplate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]int64, 0, len(r.items))
	for id := range r.items {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]*domain.NotificationTemplate, 0, len(ids))
	for _, id := range ids {
		copy := *r.items[id]
		out = append(out, &copy)
	}
	return out, nil
}

func (r *notifHTemplateRepo) Update(_ context.Context, template *domain.NotificationTemplate) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[template.ID]; !ok {
		return ports.ErrNotFound
	}
	template.UpdatedAt = time.Now().UTC()
	copy := *template
	r.items[template.ID] = &copy
	return nil
}

func (r *notifHTemplateRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[id]; !ok {
		return ports.ErrNotFound
	}
	delete(r.items, id)
	return nil
}

func newNotifHMonLinkRepo() *notifHMonLinkRepo {
	return &notifHMonLinkRepo{}
}

func (r *notifHMonLinkRepo) Attach(_ context.Context, monitorID, notificationID int64, includeTarget bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.links {
		if l.MonitorID == monitorID && l.NotificationID == notificationID {
			l.IncludeTarget = includeTarget
			return nil // idempotent
		}
	}
	r.nextID++
	r.links = append(r.links, domain.MonitorNotification{
		ID: r.nextID, MonitorID: monitorID, NotificationID: notificationID, IncludeTarget: includeTarget,
	})
	return nil
}

func (r *notifHMonLinkRepo) SetIncludeTarget(_ context.Context, monitorID, notificationID int64, includeTarget bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.links {
		if r.links[i].MonitorID == monitorID && r.links[i].NotificationID == notificationID {
			r.links[i].IncludeTarget = includeTarget
		}
	}
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
	templates  *notifHTemplateRepo
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
	templateRepo := newNotifHTemplateRepo()
	sender := &notifHSender{}

	notifSvc := services.NewNotificationService(repo, monLinks)
	notifSvc.SetTemplateRepository(templateRepo)
	notifSvc.SetGroupNotificationRepo(groupLinks)
	notifSvc.SetAssignmentLookups(monitorRepo, groupRepo)
	notifSvc.RegisterSender(sender)

	accessSvc := services.NewAccessService(userRepo, permRepo, groupRepo, monitorRepo)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	// Wiring MUST mirror internal/adapters/http/router.go so middleware gates
	// are under test, not only the in-handler requireManageNotifications checks.
	requireNotifications := middleware.RequireCapability(accessSvc, middleware.CapManageNotifications)
	nh := handlers.NewNotificationHandlers(notifSvc, accessSvc)
	templateSvc := services.NewNotificationTemplateService(templateRepo)
	th := handlers.NewNotificationTemplateHandlers(templateSvc, accessSvc)

	notifGroup := e.Group("/api/notifications", middleware.AuthMiddleware(authSvc))
	notifGroup.POST("", nh.Create, requireNotifications)
	notifGroup.GET("", nh.List)
	notifGroup.GET("/:id", nh.GetByID)
	notifGroup.GET("/:id/assignments", nh.ListAssignments)
	notifGroup.PUT("/:id", nh.Update, requireNotifications)
	notifGroup.DELETE("/:id", nh.Delete, requireNotifications)
	notifGroup.POST("/:id/test", nh.Test, requireNotifications)
	notifGroup.POST("/:id/monitor/:monitorId", nh.AttachToMonitor, requireNotifications)
	notifGroup.PUT("/:id/monitor/:monitorId", nh.UpdateMonitorLink, requireNotifications)
	notifGroup.DELETE("/:id/monitor/:monitorId", nh.DetachFromMonitor, requireNotifications)
	notifGroup.POST("/:id/group/:groupId", nh.AttachToGroup, requireNotifications)
	notifGroup.DELETE("/:id/group/:groupId", nh.DetachFromGroup, requireNotifications)

	monNotifGroup := e.Group("/api/monitors/:id/notifications", middleware.AuthMiddleware(authSvc))
	monNotifGroup.GET("", nh.ListForMonitor)

	groupNotifGroup := e.Group("/api/monitor-groups/:id/notifications", middleware.AuthMiddleware(authSvc))
	groupNotifGroup.GET("", nh.ListForGroup)

	templateGroup := e.Group("/api/notification-templates", middleware.AuthMiddleware(authSvc), requireNotifications)
	templateGroup.POST("", th.Create)
	templateGroup.GET("", th.List)
	templateGroup.GET("/variables", th.Variables)
	templateGroup.GET("/:id", th.GetByID)
	templateGroup.PUT("/:id", th.Update)
	templateGroup.DELETE("/:id", th.Delete)

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
		templates:  templateRepo,
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
	if view["include_ack_url"] != false {
		t.Errorf("include_ack_url = %v; want false (default when omitted)", view["include_ack_url"])
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
	if stored.Name != "Pager webhook" || stored.Type != "webhook" || !stored.IsDefault || !stored.Active || stored.IncludeAckURL {
		t.Errorf("stored = %+v; want name/type/is_default/active matching request and include_ack_url=false", stored)
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

func decodeAssignments(t *testing.T, rec *httptest.ResponseRecorder) (monitors, groups []map[string]any) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode assignments: %v (%s)", err, rec.Body.String())
	}
	monRaw, _ := body["monitors"].([]any)
	grpRaw, _ := body["groups"].([]any)
	for _, item := range monRaw {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("monitor assignment not an object: %v", item)
		}
		monitors = append(monitors, row)
	}
	for _, item := range grpRaw {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("group assignment not an object: %v", item)
		}
		groups = append(groups, row)
	}
	if monitors == nil {
		monitors = []map[string]any{}
	}
	if groups == nil {
		groups = []map[string]any{}
	}
	return monitors, groups
}

func assignmentIDs(rows []map[string]any) []int64 {
	out := make([]int64, 0, len(rows))
	for _, row := range rows {
		id, _ := row["id"].(float64)
		out = append(out, int64(id))
	}
	return out
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestNotificationHandlers_ListAssignments_EffectAndRBAC asserts reverse
// assignment listing reflects attach/detach (AGENTS rule 7) and never leaks
// monitors or folders the caller cannot view.
func TestNotificationHandlers_ListAssignments_EffectAndRBAC(t *testing.T) {
	h := newNotifHTTPHarness(t)

	id := h.createNotif(t, h.tokenA, "rev-assign")
	path := "/api/notifications/" + floatToIntStr(float64(id)) + "/assignments"

	empty := h.do(t, http.MethodGet, path, h.tokenA, nil)
	if empty.Code != http.StatusOK {
		t.Fatalf("ListAssignments empty = %d; want 200 (%s)", empty.Code, empty.Body.String())
	}
	mons, grps := decodeAssignments(t, empty)
	if len(mons) != 0 || len(grps) != 0 {
		t.Fatalf("empty assignments = monitors=%v groups=%v; want both empty", mons, grps)
	}

	monAPath := "/api/notifications/" + floatToIntStr(float64(id)) +
		"/monitor/" + floatToIntStr(float64(h.monitorAID))
	monBPath := "/api/notifications/" + floatToIntStr(float64(id)) +
		"/monitor/" + floatToIntStr(float64(h.monitorBID))
	grpAPath := "/api/notifications/" + floatToIntStr(float64(id)) +
		"/group/" + floatToIntStr(float64(h.groupAID))
	grpBPath := "/api/notifications/" + floatToIntStr(float64(id)) +
		"/group/" + floatToIntStr(float64(h.groupBID))

	if rec := h.do(t, http.MethodPost, monAPath, h.tokenA, map[string]any{"include_target": false}); rec.Code != http.StatusNoContent {
		t.Fatalf("attach monitor A: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := h.do(t, http.MethodPost, monBPath, h.tokenA, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("attach monitor B: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := h.do(t, http.MethodPost, grpAPath, h.tokenA, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("attach group A: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := h.do(t, http.MethodPost, grpBPath, h.tokenA, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("attach group B: %d (%s)", rec.Code, rec.Body.String())
	}

	admin := h.do(t, http.MethodGet, path, h.tokenA, nil)
	if admin.Code != http.StatusOK {
		t.Fatalf("admin ListAssignments = %d; want 200 (%s)", admin.Code, admin.Body.String())
	}
	mons, grps = decodeAssignments(t, admin)
	if got := assignmentIDs(mons); len(got) != 2 || !containsID(got, h.monitorAID) || !containsID(got, h.monitorBID) {
		t.Fatalf("admin monitors = %v; want [%d %d]", got, h.monitorAID, h.monitorBID)
	}
	if got := assignmentIDs(grps); len(got) != 2 || !containsID(got, h.groupAID) || !containsID(got, h.groupBID) {
		t.Fatalf("admin groups = %v; want [%d %d]", got, h.groupAID, h.groupBID)
	}

	var includeA any
	for _, row := range mons {
		if int64(row["id"].(float64)) == h.monitorAID {
			includeA = row["include_target"]
			if row["name"] != "a-monitor" {
				t.Errorf("monitor A name = %v; want a-monitor", row["name"])
			}
		}
		if int64(row["id"].(float64)) == h.monitorBID {
			if row["include_target"] != true {
				t.Errorf("monitor B include_target = %v; want true (default)", row["include_target"])
			}
			if row["name"] != "b-monitor" {
				t.Errorf("monitor B name = %v; want b-monitor", row["name"])
			}
		}
	}
	if includeA != false {
		t.Errorf("monitor A include_target = %v; want false", includeA)
	}

	// Capability holder B may manage the notification but must not see A.
	scoped := h.do(t, http.MethodGet, path, h.tokenB, nil)
	if scoped.Code != http.StatusOK {
		t.Fatalf("capability holder ListAssignments = %d; want 200 (%s)", scoped.Code, scoped.Body.String())
	}
	mons, grps = decodeAssignments(t, scoped)
	if got := assignmentIDs(mons); len(got) != 1 || got[0] != h.monitorBID {
		t.Fatalf("capability holder monitors = %v; want [%d] (B only, no leak of A)", got, h.monitorBID)
	}
	if got := assignmentIDs(grps); len(got) != 1 || got[0] != h.groupBID {
		t.Fatalf("capability holder groups = %v; want [%d] (B only, no leak of A)", got, h.groupBID)
	}

	// Non-manager C can read assignments of a notification attached to a
	// visible monitor, still filtered to what they can see.
	reader := h.do(t, http.MethodGet, path, h.tokenC, nil)
	if reader.Code != http.StatusOK {
		t.Fatalf("non-manager ListAssignments = %d; want 200 (%s)", reader.Code, reader.Body.String())
	}
	mons, grps = decodeAssignments(t, reader)
	if got := assignmentIDs(mons); len(got) != 1 || got[0] != h.monitorBID {
		t.Fatalf("non-manager monitors = %v; want [%d]", got, h.monitorBID)
	}
	if got := assignmentIDs(grps); len(got) != 1 || got[0] != h.groupBID {
		t.Fatalf("non-manager groups = %v; want [%d]", got, h.groupBID)
	}

	if rec := h.do(t, http.MethodDelete, monBPath, h.tokenA, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("detach monitor B: %d (%s)", rec.Code, rec.Body.String())
	}
	after := h.do(t, http.MethodGet, path, h.tokenA, nil)
	if after.Code != http.StatusOK {
		t.Fatalf("ListAssignments after detach = %d; want 200", after.Code)
	}
	mons, _ = decodeAssignments(t, after)
	if got := assignmentIDs(mons); containsID(got, h.monitorBID) {
		t.Fatalf("after detach, monitors still contain B: %v", got)
	}

	unknown := h.do(t, http.MethodGet, "/api/notifications/99999/assignments", h.tokenA, nil)
	if unknown.Code != http.StatusNotFound {
		t.Errorf("unknown notification ListAssignments = %d; want 404", unknown.Code)
	}

	unauth := h.do(t, http.MethodGet, path, "", nil)
	if unauth.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated ListAssignments = %d; want 401", unauth.Code)
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

// TestNotificationHandlers_CreateAndUpdate_IncludeAckURLToggle asserts the
// acknowledgement-link flag defaults on, can be created off, and can be
// flipped on update. The effect is the persisted row, not the status code.
func TestNotificationHandlers_CreateAndUpdate_IncludeAckURLToggle(t *testing.T) {
	h := newNotifHTTPHarness(t)
	ctx := context.Background()

	include := true
	rec := h.do(t, http.MethodPost, "/api/notifications", h.tokenB, map[string]any{
		"name":            "ack-pager",
		"type":            "webhook",
		"include_ack_url": include,
		"config":          map[string]any{"url": "https://hooks.example/ack"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST with include_ack_url=true returned %d; want 201 (%s)", rec.Code, rec.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if view["include_ack_url"] != true {
		t.Errorf("create view include_ack_url = %v; want true", view["include_ack_url"])
	}
	id := int64(view["id"].(float64))
	stored, err := h.svc.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID after create: %v", err)
	}
	if !stored.IncludeAckURL {
		t.Fatal("stored IncludeAckURL = false after explicit true create")
	}

	disable := false
	updateRec := h.do(t, http.MethodPut, "/api/notifications/"+floatToIntStr(float64(id)), h.tokenB, map[string]any{
		"include_ack_url": disable,
	})
	if updateRec.Code != http.StatusOK {
		t.Fatalf("PUT include_ack_url=false returned %d; want 200 (%s)", updateRec.Code, updateRec.Body.String())
	}
	updated, err := h.svc.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if updated.IncludeAckURL {
		t.Fatal("stored IncludeAckURL = true after update to false")
	}
}

// TestNotificationHandlers_MonitorLink_IncludeTargetToggle asserts the per-link
// include_target flag defaults true on attach, is exposed by the monitor detail
// read path, and flips via PUT.
func TestNotificationHandlers_MonitorLink_IncludeTargetToggle(t *testing.T) {
	h := newNotifHTTPHarness(t)
	ctx := context.Background()

	id := h.createNotif(t, h.tokenB, "target-link")
	path := "/api/notifications/" + floatToIntStr(float64(id)) +
		"/monitor/" + floatToIntStr(float64(h.monitorBID))

	// Attach with no body → include_target defaults true.
	attach := h.do(t, http.MethodPost, path, h.tokenB, nil)
	if attach.Code != http.StatusNoContent {
		t.Fatalf("AttachToMonitor returned %d; want 204 (%s)", attach.Code, attach.Body.String())
	}

	listRec := h.do(t, http.MethodGet,
		"/api/monitors/"+floatToIntStr(float64(h.monitorBID))+"/notifications", h.tokenB, nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("ListForMonitor = %d; want 200", listRec.Code)
	}
	var monNotifs []map[string]any
	if err := json.Unmarshal(listRec.Body.Bytes(), &monNotifs); err != nil {
		t.Fatalf("decode ListForMonitor: %v", err)
	}
	if len(monNotifs) != 1 || monNotifs[0]["include_target"] != true {
		t.Fatalf("ListForMonitor after attach = %v; want one link with include_target=true", monNotifs)
	}

	// Explicit false.
	update := h.do(t, http.MethodPut, path, h.tokenB, map[string]any{"include_target": false})
	if update.Code != http.StatusNoContent {
		t.Fatalf("PUT include_target=false returned %d; want 204 (%s)", update.Code, update.Body.String())
	}
	links, err := h.monLinks.ListByMonitor(ctx, h.monitorBID)
	if err != nil {
		t.Fatalf("ListByMonitor: %v", err)
	}
	if len(links) != 1 || links[0].IncludeTarget {
		t.Fatalf("after PUT false = %#v; want IncludeTarget=false", links)
	}

	// Explicit true flips it back.
	update = h.do(t, http.MethodPut, path, h.tokenB, map[string]any{"include_target": true})
	if update.Code != http.StatusNoContent {
		t.Fatalf("PUT include_target=true returned %d; want 204 (%s)", update.Code, update.Body.String())
	}
	links, _ = h.monLinks.ListByMonitor(ctx, h.monitorBID)
	if len(links) != 1 || !links[0].IncludeTarget {
		t.Fatalf("after PUT true = %#v; want IncludeTarget=true", links)
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

func TestNotificationTemplateHandlers_CRUDVariablesAndAssignmentValidation(t *testing.T) {
	h := newNotifHTTPHarness(t)

	denied := h.do(t, http.MethodPost, "/api/notification-templates", h.tokenC, map[string]any{
		"name": "Read-only attempt", "provider": "discord", "body_template": "{{ message }}",
	})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("read-only POST template = %d; want 403 (body: %s)", denied.Code, denied.Body.String())
	}

	created := h.do(t, http.MethodPost, "/api/notification-templates", h.tokenB, map[string]any{
		"name":           "Discord incident",
		"provider":       "discord",
		"title_template": "{{ status.emoji }} {{ alert.name }} is {{ status }}",
		"body_template":  "{{ message }}\n{{ ack_url }}",
		"discord_config": map[string]any{
			"title_url_template": "{{ alert.target }}",
			"footer_template":    "Phoenix • {{ alert.scope }}",
			"show_timestamp":     true,
			"colors": map[string]any{
				"up": "#00FF00", "down": "#FF0000", "pending": "#FFA500",
				"maintenance": "#808080", "certificate": "#FFA500",
			},
			"fields": []any{map[string]any{
				"name_template": "Condition", "value_template": "{{ group.condition }}", "inline": true,
			}},
		},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("POST template = %d; want 201 (body: %s)", created.Code, created.Body.String())
	}
	var templateView struct {
		ID            int64                               `json:"id"`
		Provider      string                              `json:"provider"`
		DiscordConfig *handlers.DiscordTemplateConfigView `json:"discord_config"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &templateView); err != nil {
		t.Fatalf("decode created template: %v", err)
	}
	if templateView.ID == 0 || templateView.Provider != "discord" {
		t.Fatalf("created template = %+v", templateView)
	}
	if templateView.DiscordConfig == nil || len(templateView.DiscordConfig.Fields) != 1 || templateView.DiscordConfig.Colors.Down != "#FF0000" {
		t.Fatalf("created Discord config = %+v", templateView.DiscordConfig)
	}

	variables := h.do(t, http.MethodGet, "/api/notification-templates/variables", h.tokenB, nil)
	if variables.Code != http.StatusOK || !bytes.Contains(variables.Body.Bytes(), []byte(`"monitor.name"`)) || !bytes.Contains(variables.Body.Bytes(), []byte(`"group.condition"`)) {
		t.Fatalf("GET variables = %d %s; want monitor and group variables", variables.Code, variables.Body.String())
	}

	mismatch := h.do(t, http.MethodPost, "/api/notifications", h.tokenB, map[string]any{
		"name": "Webhook with Discord template", "type": "webhook",
		"template_id": templateView.ID,
		"config":      map[string]any{"url": "https://hooks.example.test"},
	})
	if mismatch.Code != http.StatusBadRequest {
		t.Fatalf("POST mismatched notification = %d; want 400 (body: %s)", mismatch.Code, mismatch.Body.String())
	}

	matching := h.do(t, http.MethodPost, "/api/notifications", h.tokenB, map[string]any{
		"name": "Discord channel", "type": "discord",
		"template_id": templateView.ID,
		"config":      map[string]any{"webhook_url": "https://discord.example.test"},
	})
	if matching.Code != http.StatusCreated {
		t.Fatalf("POST matching notification = %d; want 201 (body: %s)", matching.Code, matching.Body.String())
	}
	var notificationView struct {
		TemplateID *int64 `json:"template_id"`
	}
	if err := json.Unmarshal(matching.Body.Bytes(), &notificationView); err != nil {
		t.Fatalf("decode matching notification: %v", err)
	}
	if notificationView.TemplateID == nil || *notificationView.TemplateID != templateView.ID {
		t.Fatalf("notification template_id = %v; want %d", notificationView.TemplateID, templateView.ID)
	}

	updated := h.do(t, http.MethodPut, "/api/notification-templates/"+strconv.FormatInt(templateView.ID, 10), h.tokenB, map[string]any{
		"name": "Discord incident v2", "title_template": "{{ monitor.name }}", "body_template": "{{ check_output }}",
	})
	if updated.Code != http.StatusOK || !bytes.Contains(updated.Body.Bytes(), []byte(`"provider":"discord"`)) {
		t.Fatalf("PUT template = %d %s; want immutable discord provider", updated.Code, updated.Body.String())
	}
	if !bytes.Contains(updated.Body.Bytes(), []byte(`"footer_template":"Phoenix • {{ alert.scope }}"`)) {
		t.Fatalf("PUT without discord_config should preserve existing embed config: %s", updated.Body.String())
	}

	smtpCreated := h.do(t, http.MethodPost, "/api/notification-templates", h.tokenB, map[string]any{
		"name":           "Rich email",
		"provider":       "smtp",
		"title_template": "{{ alert.name }} is {{ status }}",
		"body_template":  "{{ alert.name }} is {{ status }}\n{{ message }}",
		"smtp_config": map[string]any{
			"format":             "html",
			"html_body_template": `<h1>{{ alert.name }}</h1><p>{{ message }}</p>`,
		},
	})
	if smtpCreated.Code != http.StatusCreated {
		t.Fatalf("POST SMTP template = %d; want 201 (body: %s)", smtpCreated.Code, smtpCreated.Body.String())
	}
	var smtpTemplateView struct {
		ID         int64                            `json:"id"`
		Provider   string                           `json:"provider"`
		SMTPConfig *handlers.SMTPTemplateConfigView `json:"smtp_config"`
	}
	if err := json.Unmarshal(smtpCreated.Body.Bytes(), &smtpTemplateView); err != nil {
		t.Fatalf("decode created SMTP template: %v", err)
	}
	if smtpTemplateView.ID == 0 || smtpTemplateView.Provider != "smtp" || smtpTemplateView.SMTPConfig == nil ||
		smtpTemplateView.SMTPConfig.Format != "html" || !strings.Contains(smtpTemplateView.SMTPConfig.HTMLBodyTemplate, "<h1>") {
		t.Fatalf("created SMTP template = %+v", smtpTemplateView)
	}

	smtpUpdated := h.do(t, http.MethodPut, "/api/notification-templates/"+strconv.FormatInt(smtpTemplateView.ID, 10), h.tokenB, map[string]any{
		"name": "Rich email v2", "title_template": "{{ alert.name }}", "body_template": "Fallback: {{ message }}",
	})
	var preservedSMTPTemplate struct {
		SMTPConfig *handlers.SMTPTemplateConfigView `json:"smtp_config"`
	}
	if err := json.Unmarshal(smtpUpdated.Body.Bytes(), &preservedSMTPTemplate); err != nil {
		t.Fatalf("decode updated SMTP template: %v", err)
	}
	if smtpUpdated.Code != http.StatusOK || preservedSMTPTemplate.SMTPConfig == nil ||
		preservedSMTPTemplate.SMTPConfig.Format != "html" ||
		preservedSMTPTemplate.SMTPConfig.HTMLBodyTemplate != `<h1>{{ alert.name }}</h1><p>{{ message }}</p>` {
		t.Fatalf("PUT without smtp_config should preserve existing email config: %d %s", smtpUpdated.Code, smtpUpdated.Body.String())
	}

	wrongSMTPConfig := h.do(t, http.MethodPost, "/api/notification-templates", h.tokenB, map[string]any{
		"name": "LINE with email config", "provider": "line", "body_template": "{{ message }}",
		"smtp_config": map[string]any{"format": "plain", "html_body_template": ""},
	})
	if wrongSMTPConfig.Code != http.StatusBadRequest {
		t.Fatalf("POST mismatched smtp_config = %d; want 400 (body: %s)", wrongSMTPConfig.Code, wrongSMTPConfig.Body.String())
	}

	deleted := h.do(t, http.MethodDelete, "/api/notification-templates/"+strconv.FormatInt(templateView.ID, 10), h.tokenB, nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("DELETE template = %d; want 204 (body: %s)", deleted.Code, deleted.Body.String())
	}
}
