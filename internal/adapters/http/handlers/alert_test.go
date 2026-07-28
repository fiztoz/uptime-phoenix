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
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type alertHandlerRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.Alert
	nextID int64
}

func newAlertHandlerRepo() *alertHandlerRepo {
	return &alertHandlerRepo{byID: make(map[int64]*domain.Alert), nextID: 1}
}

func cloneHandlerAlert(a *domain.Alert) *domain.Alert {
	if a == nil {
		return nil
	}
	cp := *a
	if a.AckedAt != nil {
		v := *a.AckedAt
		cp.AckedAt = &v
	}
	if a.AckedByUserID != nil {
		v := *a.AckedByUserID
		cp.AckedByUserID = &v
	}
	if a.ResolvedAt != nil {
		v := *a.ResolvedAt
		cp.ResolvedAt = &v
	}
	if a.OpenMonitorID != nil {
		v := *a.OpenMonitorID
		cp.OpenMonitorID = &v
	}
	return &cp
}

func (r *alertHandlerRepo) Create(_ context.Context, a *domain.Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.byID {
		if existing.AckToken == a.AckToken {
			return ports.ErrConflict
		}
		if existing.OpenMonitorID != nil && a.OpenMonitorID != nil &&
			*existing.OpenMonitorID == *a.OpenMonitorID {
			return ports.ErrConflict
		}
	}
	a.ID = r.nextID
	r.nextID++
	now := time.Now().UTC()
	a.CreatedAt = now
	a.UpdatedAt = now
	r.byID[a.ID] = cloneHandlerAlert(a)
	return nil
}

func (r *alertHandlerRepo) Update(_ context.Context, a *domain.Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[a.ID]; !ok {
		return ports.ErrNotFound
	}
	a.UpdatedAt = time.Now().UTC()
	r.byID[a.ID] = cloneHandlerAlert(a)
	return nil
}

func (r *alertHandlerRepo) GetByID(_ context.Context, id int64) (*domain.Alert, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return cloneHandlerAlert(a), nil
}

func (r *alertHandlerRepo) GetByAckToken(_ context.Context, token string) (*domain.Alert, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.byID {
		if a.AckToken == token {
			return cloneHandlerAlert(a), nil
		}
	}
	return nil, ports.ErrNotFound
}

func (r *alertHandlerRepo) GetOpenByMonitorID(_ context.Context, monitorID int64) (*domain.Alert, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.byID {
		if a.OpenMonitorID != nil && *a.OpenMonitorID == monitorID {
			return cloneHandlerAlert(a), nil
		}
	}
	return nil, ports.ErrNotFound
}

func (r *alertHandlerRepo) List(_ context.Context, filter ports.AlertFilter) ([]*domain.Alert, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	allowed := make(map[int64]bool, len(filter.MonitorIDs))
	for _, id := range filter.MonitorIDs {
		allowed[id] = true
	}
	statuses := make(map[string]bool, len(filter.Statuses))
	for _, status := range filter.Statuses {
		statuses[status] = true
	}
	out := make([]*domain.Alert, 0, len(r.byID))
	for _, a := range r.byID {
		if filter.RestrictToMonitorIDs && !allowed[a.MonitorID] {
			continue
		}
		if filter.MonitorID != nil && a.MonitorID != *filter.MonitorID {
			continue
		}
		if filter.OpenOnly && !a.IsOpen() {
			continue
		}
		if len(statuses) > 0 && !statuses[a.Status] {
			continue
		}
		out = append(out, cloneHandlerAlert(a))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FiredAt.Equal(out[j].FiredAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].FiredAt.After(out[j].FiredAt)
	})
	if filter.Offset >= len(out) {
		return []*domain.Alert{}, nil
	}
	if filter.Offset > 0 {
		out = out[filter.Offset:]
	}
	if filter.Limit > 0 && filter.Limit < len(out) {
		out = out[:filter.Limit]
	}
	return out, nil
}

type alertHTTPHarness struct {
	router *echo.Echo
	repo   *alertHandlerRepo
	svc    *services.AlertService

	adminToken  string
	memberToken string
	memberID    int64

	visibleAlert *domain.Alert
	hiddenAlert  *domain.Alert
}

func newAlertHTTPHarness(t *testing.T) *alertHTTPHarness {
	t.Helper()
	ctx := context.Background()

	userRepo := memory.NewUserRepo()
	apiKeyRepo := memory.NewAPIKeyRepo()
	permRepo := memory.NewUserPermissionRepo()
	authenticator := auth.NewJWTAuthenticator("alert-handler-test-key", 24, userRepo)
	authSvc := services.NewAuthService(
		userRepo,
		apiKeyRepo,
		authenticator,
		auth.NewTOTPProvider("Phoenix"),
	)
	if _, err := authSvc.Register(ctx, "alert-admin", "password123"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	adminToken, loginAdminErr := authSvc.Login(ctx, "alert-admin", "password123")
	if loginAdminErr != nil {
		t.Fatalf("login admin: %v", loginAdminErr)
	}
	member, createMemberErr := authSvc.CreateUser(
		ctx,
		"alert-member",
		"password123",
		true,
		false,
		"UTC",
		services.UserCapabilities{},
	)
	if createMemberErr != nil {
		t.Fatalf("create member: %v", createMemberErr)
	}
	memberToken, loginMemberErr := authSvc.Login(ctx, "alert-member", "password123")
	if loginMemberErr != nil {
		t.Fatalf("login member: %v", loginMemberErr)
	}

	monitorRepo := newFakeMonitorRepo()
	visibleMonitor := &domain.Monitor{
		UserID: 1, Name: "visible-api", Type: "http", Active: true, Interval: 60,
	}
	hiddenMonitor := &domain.Monitor{
		UserID: 1, Name: "hidden-api", Type: "http", Active: true, Interval: 60,
	}
	if err := monitorRepo.Create(ctx, visibleMonitor); err != nil {
		t.Fatalf("create visible monitor: %v", err)
	}
	if err := monitorRepo.Create(ctx, hiddenMonitor); err != nil {
		t.Fatalf("create hidden monitor: %v", err)
	}
	if err := permRepo.Grant(ctx, &domain.UserPermission{
		UserID:    member.ID,
		MonitorID: &visibleMonitor.ID,
	}); err != nil {
		t.Fatalf("grant visible monitor: %v", err)
	}

	repo := newAlertHandlerRepo()
	alertSvc := services.NewAlertService(repo)
	firedAt := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	visibleAlert, openVisibleErr := alertSvc.OpenOnDown(ctx, visibleMonitor, firedAt)
	if openVisibleErr != nil {
		t.Fatalf("open visible alert: %v", openVisibleErr)
	}
	hiddenAlert, openHiddenErr := alertSvc.OpenOnDown(ctx, hiddenMonitor, firedAt.Add(time.Minute))
	if openHiddenErr != nil {
		t.Fatalf("open hidden alert: %v", openHiddenErr)
	}

	accessSvc := services.NewAccessService(userRepo, permRepo, nil, monitorRepo)
	alertHandlers := handlers.NewAlertHandlers(alertSvc, accessSvc)
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.POST("/api/alerts/ack-by-token", alertHandlers.AcknowledgeByToken)
	group := e.Group("/api/alerts", middleware.AuthMiddleware(authSvc))
	group.GET("", alertHandlers.List)
	group.GET("/:id", alertHandlers.Get)
	group.POST("/:id/ack", alertHandlers.Acknowledge)

	return &alertHTTPHarness{
		router:       e,
		repo:         repo,
		svc:          alertSvc,
		adminToken:   adminToken,
		memberToken:  memberToken,
		memberID:     member.ID,
		visibleAlert: visibleAlert,
		hiddenAlert:  hiddenAlert,
	}
}

func (h *alertHTTPHarness) do(
	t *testing.T,
	method string,
	path string,
	token string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if token != "" {
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func assertAlertWireHasNoSecrets(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var view map[string]any
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode alert view: %v", err)
	}
	for _, forbidden := range []string{
		"ack_token",
		"open_monitor_id",
		"AckToken",
		"OpenMonitorID",
		"MonitorID",
	} {
		if _, exists := view[forbidden]; exists {
			t.Fatalf("alert response leaked forbidden field %q: %s", forbidden, body)
		}
	}
	for _, required := range []string{"id", "monitor_id", "status", "fired_at", "created_at", "updated_at"} {
		if _, exists := view[required]; !exists {
			t.Fatalf("alert response is missing wire field %q: %s", required, body)
		}
	}
	return view
}

func TestAlertHandlers_ListScopesAndDoesNotLeakTokens(t *testing.T) {
	h := newAlertHTTPHarness(t)

	rec := h.do(t, http.MethodGet, "/api/alerts?open=1", h.memberToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("member list = %d; want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var memberViews []json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &memberViews); err != nil {
		t.Fatalf("decode member list: %v", err)
	}
	if len(memberViews) != 1 {
		t.Fatalf("member list has %d alerts; want exactly the granted monitor's alert", len(memberViews))
	}
	view := assertAlertWireHasNoSecrets(t, memberViews[0])
	if got := int64(view["monitor_id"].(float64)); got != h.visibleAlert.MonitorID {
		t.Fatalf("member saw monitor %d; want granted monitor %d", got, h.visibleAlert.MonitorID)
	}

	rec = h.do(t, http.MethodGet, "/api/alerts", h.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin list = %d; want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var adminViews []json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &adminViews); err != nil {
		t.Fatalf("decode admin list: %v", err)
	}
	if len(adminViews) != 2 {
		t.Fatalf("admin list has %d alerts; want both alerts", len(adminViews))
	}
	for _, raw := range adminViews {
		assertAlertWireHasNoSecrets(t, raw)
	}
}

func TestAlertHandlers_GetAndAcknowledgeEnforceMonitorVisibility(t *testing.T) {
	h := newAlertHTTPHarness(t)

	rec := h.do(
		t,
		http.MethodGet,
		"/api/alerts/"+int64String(h.visibleAlert.ID),
		h.memberToken,
		nil,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("get granted alert = %d; want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	assertAlertWireHasNoSecrets(t, rec.Body.Bytes())

	rec = h.do(
		t,
		http.MethodGet,
		"/api/alerts/"+int64String(h.hiddenAlert.ID),
		h.memberToken,
		nil,
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get hidden alert = %d; want denial-as-404", rec.Code)
	}

	rec = h.do(
		t,
		http.MethodPost,
		"/api/alerts/"+int64String(h.hiddenAlert.ID)+"/ack",
		h.memberToken,
		map[string]any{},
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ack hidden alert = %d; want denial-as-404", rec.Code)
	}
	hidden, err := h.repo.GetByID(context.Background(), h.hiddenAlert.ID)
	if err != nil {
		t.Fatalf("re-read hidden alert: %v", err)
	}
	if hidden.Status != domain.AlertStatusFiring {
		t.Fatalf("hidden alert status = %s; denied request must not mutate it", hidden.Status)
	}

	rec = h.do(
		t,
		http.MethodPost,
		"/api/alerts/"+int64String(h.visibleAlert.ID)+"/ack",
		h.memberToken,
		map[string]any{},
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("ack granted alert = %d; want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	view := assertAlertWireHasNoSecrets(t, rec.Body.Bytes())
	if view["status"] != domain.AlertStatusAcked {
		t.Fatalf("ack response status = %v; want acked", view["status"])
	}
	if got := int64(view["acked_by_user_id"].(float64)); got != h.memberID {
		t.Fatalf("acked_by_user_id = %d; want session user %d", got, h.memberID)
	}
	persisted, err := h.repo.GetByID(context.Background(), h.visibleAlert.ID)
	if err != nil {
		t.Fatalf("re-read visible alert: %v", err)
	}
	if persisted.Status != domain.AlertStatusAcked ||
		persisted.AckedByUserID == nil ||
		*persisted.AckedByUserID != h.memberID {
		t.Fatalf("ack effect was not persisted: %+v", persisted)
	}
}

func TestAlertHandlers_AcknowledgeByTokenValidatesCredentialAndKeepsItSecret(t *testing.T) {
	t.Run("valid public token", func(t *testing.T) {
		h := newAlertHTTPHarness(t)
		rec := h.do(
			t,
			http.MethodPost,
			"/api/alerts/ack-by-token",
			"",
			map[string]any{"token": "  " + h.hiddenAlert.AckToken + "  "},
		)
		if rec.Code != http.StatusOK {
			t.Fatalf("public token ack = %d; want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		view := assertAlertWireHasNoSecrets(t, rec.Body.Bytes())
		if view["status"] != domain.AlertStatusAcked {
			t.Fatalf("public token status = %v; want acked", view["status"])
		}
		if _, exists := view["acked_by_user_id"]; exists {
			t.Fatal("public token acknowledgement must not impersonate a user")
		}
		persisted, err := h.repo.GetByID(context.Background(), h.hiddenAlert.ID)
		if err != nil {
			t.Fatalf("re-read public-acked alert: %v", err)
		}
		if persisted.Status != domain.AlertStatusAcked || persisted.AckedByUserID != nil {
			t.Fatalf("public ack effect was not persisted correctly: %+v", persisted)
		}
	})

	t.Run("missing and unknown token", func(t *testing.T) {
		h := newAlertHTTPHarness(t)
		rec := h.do(
			t,
			http.MethodPost,
			"/api/alerts/ack-by-token",
			"",
			map[string]any{"token": "   "},
		)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("blank token = %d; want 400", rec.Code)
		}
		rec = h.do(
			t,
			http.MethodPost,
			"/api/alerts/ack-by-token",
			"",
			map[string]any{"token": "not-a-real-alert-token"},
		)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unknown token = %d; want 404", rec.Code)
		}
	})

	t.Run("resolved token is spent", func(t *testing.T) {
		h := newAlertHTTPHarness(t)
		if err := h.svc.ResolveOpen(
			context.Background(),
			h.hiddenAlert.MonitorID,
			time.Now().UTC(),
		); err != nil {
			t.Fatalf("resolve alert: %v", err)
		}
		rec := h.do(
			t,
			http.MethodPost,
			"/api/alerts/ack-by-token",
			"",
			map[string]any{"token": h.hiddenAlert.AckToken},
		)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("spent token = %d; want 404", rec.Code)
		}
	})
}

func TestAlertHandlers_ProtectedRoutesRequireAuthentication(t *testing.T) {
	h := newAlertHTTPHarness(t)
	rec := h.do(t, http.MethodGet, "/api/alerts", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated alert list = %d; want 401", rec.Code)
	}
}

func int64String(v int64) string {
	return strconv.FormatInt(v, 10)
}
