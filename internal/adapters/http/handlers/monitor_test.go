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
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/middleware"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// --- In-memory monitor repo for tests -----------------------------------

type fakeMonitorRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.Monitor
	byName map[string]int64
	nextID int64
}

func newFakeMonitorRepo() *fakeMonitorRepo {
	return &fakeMonitorRepo{
		byID:   make(map[int64]*domain.Monitor),
		byName: make(map[string]int64),
	}
}

func (r *fakeMonitorRepo) Create(_ context.Context, m *domain.Monitor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	m.ID = r.nextID
	now := time.Now().UTC()
	m.CreatedAt = now
	m.UpdatedAt = now
	r.byID[m.ID] = m
	r.byName[m.Name] = m.ID
	return nil
}

func (r *fakeMonitorRepo) GetByID(_ context.Context, id int64) (*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return m, nil
}

func (r *fakeMonitorRepo) GetByPushToken(_ context.Context, token string) (*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.byID {
		if m.PushToken == token {
			return m, nil
		}
	}
	return nil, ports.ErrNotFound
}

func (r *fakeMonitorRepo) List(_ context.Context, filter ports.MonitorFilter) ([]*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// RestrictToIDs must be honored here exactly as the SQL adapters honor it, or
	// the RBAC tests below would pass against a fake that silently ignores the
	// allowlist while the real repo enforces it (or, worse, the other way round).
	// Note the branch is on the FLAG, never on len(MonitorIDs): an empty allowlist
	// means "no monitors", not "no filter".
	var allowed map[int64]bool
	if filter.RestrictToIDs {
		allowed = make(map[int64]bool, len(filter.MonitorIDs))
		for _, id := range filter.MonitorIDs {
			allowed[id] = true
		}
	}

	out := make([]*domain.Monitor, 0, len(r.byID))
	for _, m := range r.byID {
		if filter.RestrictToIDs && !allowed[m.ID] {
			continue
		}
		if filter.UserID > 0 && m.UserID != filter.UserID {
			continue
		}
		if filter.Active != nil && m.Active != *filter.Active {
			continue
		}
		if filter.Type != "" && m.Type != filter.Type {
			continue
		}
		if filter.Search != "" && !contains(m.Name, filter.Search) {
			continue
		}
		if filter.GroupIDIsNull && m.GroupID != nil {
			continue
		}
		if filter.GroupID != nil && (m.GroupID == nil || *m.GroupID != *filter.GroupID) {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *fakeMonitorRepo) ListActive(_ context.Context) ([]*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.Monitor
	for _, m := range r.byID {
		if m.Active {
			out = append(out, m)
		}
	}
	return out, nil
}

func (r *fakeMonitorRepo) Update(_ context.Context, m *domain.Monitor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[m.ID]; !ok {
		return ports.ErrNotFound
	}
	m.UpdatedAt = time.Now().UTC()
	r.byID[m.ID] = m
	return nil
}

func (r *fakeMonitorRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[id]; !ok {
		return ports.ErrNotFound
	}
	delete(r.byID, id)
	return nil
}

func (r *fakeMonitorRepo) ClaimBatch(_ context.Context, _ string, _ int, _ time.Duration) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *fakeMonitorRepo) RefreshLease(_ context.Context, _ string) (int64, error)  { return 0, nil }
func (r *fakeMonitorRepo) ReleaseLeases(_ context.Context, _ string) (int64, error) { return 0, nil }

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// fakeBus for monitor tests.
type fakeMonitorBus struct {
	mu     sync.Mutex
	events []ports.Event
}

func newFakeMonitorBus() *fakeMonitorBus {
	return &fakeMonitorBus{events: make([]ports.Event, 0)}
}

func (b *fakeMonitorBus) Publish(_ context.Context, event ports.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, event)
	return nil
}

func (b *fakeMonitorBus) Subscribe(eventType string) <-chan ports.Event {
	ch := make(chan ports.Event, 100)
	return ch
}

func (b *fakeMonitorBus) Close() {}

// --- Test harness -------------------------------------------------------

type monitorTestHarness struct {
	router *echo.Echo
	svc    *services.MonitorService
	repo   *fakeMonitorRepo
	token  string // JWT for auth
}

func newMonitorHarness(t *testing.T) *monitorTestHarness {
	t.Helper()

	// User repo + auth for token generation.
	userRepo := memory.NewUserRepo()
	apiKeyRepo := memory.NewAPIKeyRepo()
	authenticator := auth.NewJWTAuthenticator("test-key", 24, userRepo)
	totp := auth.NewTOTPProvider("Phoenix")
	authSvc := services.NewAuthService(userRepo, apiKeyRepo, authenticator, totp)

	// Register a test user and get a token.
	ctx := context.Background()
	_, err := authSvc.Register(ctx, "testuser", "password123")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Pre-populate the fake authenticator's hash.
	// The auth service's Register uses userRepo, and the authenticator
	// reads the hash from userRepo. Our fake JWT adapter also reads from
	// userRepo. So we need to seed the in-memory hash.
	// Actually, Register stores it via userRepo, and Login reads via
	// userRepo then verifies via the authenticator. The fake authenticator
	// in our test setup uses the user's stored hash.
	// But the production JWTAuthenticator stores a separate user map.
	// Since we're using the real JWTAuthenticator, it reads from userRepo,
	// so Login should work.
	token, err := authSvc.Login(ctx, "testuser", "password123")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Initialize the checker registry (stubs registered via init()).
	_ = checker.Get

	monitorRepo := newFakeMonitorRepo()
	bus := newFakeMonitorBus()
	monitorSvc := services.NewMonitorService(monitorRepo, bus)

	// The harness user was created via Register, which makes the first user an
	// admin — so this harness exercises the single-admin install: the access
	// service says "sees everything, may do everything", and every assertion in
	// this file must behave exactly as it did before RBAC existed.
	accessSvc := services.NewAccessService(userRepo, memory.NewUserPermissionRepo(), nil, monitorRepo)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	monitorH := handlers.NewMonitorHandlers(monitorSvc, accessSvc, nil, nil)
	monitorGroup := e.Group("/api/monitors", middleware.AuthMiddleware(authSvc))
	monitorGroup.POST("", monitorH.Create)
	monitorGroup.GET("", monitorH.List)
	monitorGroup.GET("/:id", monitorH.GetByID)
	monitorGroup.PUT("/:id", monitorH.Update)
	monitorGroup.DELETE("/:id", monitorH.Delete)

	return &monitorTestHarness{
		router: e,
		svc:    monitorSvc,
		repo:   monitorRepo,
		token:  token,
	}
}

func (h *monitorTestHarness) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
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

// --- Tests ---------------------------------------------------------------

func TestMonitorHandlers_Create(t *testing.T) {
	h := newMonitorHarness(t)

	body := map[string]any{
		"name":     "Test Monitor",
		"type":     "http",
		"interval": 60,
		"timeout":  30,
		"config":   map[string]any{"url": "https://example.com"},
	}

	rec := h.do(t, http.MethodPost, "/api/monitors", body)
	if rec.Code != http.StatusCreated {
		t.Errorf("POST /api/monitors returned %d; want 201", rec.Code)
	}

	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if view["name"] != "Test Monitor" {
		t.Errorf("name = %v; want Test Monitor", view["name"])
	}
	if view["type"] != "http" {
		t.Errorf("type = %v; want http", view["type"])
	}
	if id, ok := view["id"].(float64); !ok || id <= 0 {
		t.Errorf("id should be a positive number, got %v", view["id"])
	}
}

func TestMonitorHandlers_Create_MissingName(t *testing.T) {
	h := newMonitorHarness(t)

	body := map[string]any{
		"type":   "http",
		"config": map[string]any{"url": "https://example.com"},
	}

	rec := h.do(t, http.MethodPost, "/api/monitors", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /api/monitors (no name) returned %d; want 400", rec.Code)
	}
}

func TestMonitorHandlers_Create_InvalidConfig(t *testing.T) {
	h := newMonitorHarness(t)

	body := map[string]any{
		"name":   "Bad Monitor",
		"type":   "http",
		"config": map[string]any{}, // missing url
	}

	rec := h.do(t, http.MethodPost, "/api/monitors", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /api/monitors (invalid config) returned %d; want 400", rec.Code)
	}
}

func TestMonitorHandlers_List(t *testing.T) {
	h := newMonitorHarness(t)

	// Create two monitors.
	for _, name := range []string{"Monitor A", "Monitor B"} {
		body := map[string]any{
			"name":   name,
			"type":   "http",
			"config": map[string]any{"url": "https://example.com"},
		}
		rec := h.do(t, http.MethodPost, "/api/monitors", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d", name, rec.Code)
		}
	}

	rec := h.do(t, http.MethodGet, "/api/monitors", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /api/monitors returned %d; want 200", rec.Code)
	}

	var monitors []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &monitors); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(monitors) != 2 {
		t.Errorf("got %d monitors; want 2", len(monitors))
	}
}

func TestMonitorHandlers_GetByID(t *testing.T) {
	h := newMonitorHarness(t)

	// Create a monitor.
	body := map[string]any{
		"name":   "Test Monitor",
		"type":   "http",
		"config": map[string]any{"url": "https://example.com"},
	}
	createRec := h.do(t, http.MethodPost, "/api/monitors", body)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: %d", createRec.Code)
	}
	var created map[string]any
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	id := created["id"].(float64)

	// Get by ID.
	rec := h.do(t, http.MethodGet, "/api/monitors/"+floatToIntStr(id), nil)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /api/monitors/:id returned %d; want 200", rec.Code)
	}

	var monitor map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &monitor); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if monitor["name"] != "Test Monitor" {
		t.Errorf("name = %v; want Test Monitor", monitor["name"])
	}
}

func TestMonitorHandlers_GetByID_NotFound(t *testing.T) {
	h := newMonitorHarness(t)
	rec := h.do(t, http.MethodGet, "/api/monitors/999", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/monitors/999 returned %d; want 404", rec.Code)
	}
}

func TestMonitorHandlers_Update(t *testing.T) {
	h := newMonitorHarness(t)

	// Create.
	body := map[string]any{
		"name":   "Original",
		"type":   "http",
		"config": map[string]any{"url": "https://example.com"},
	}
	createRec := h.do(t, http.MethodPost, "/api/monitors", body)
	var created map[string]any
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	id := created["id"].(float64)

	// Update.
	updateBody := map[string]any{
		"name": "Updated Monitor",
	}
	rec := h.do(t, http.MethodPut, "/api/monitors/"+floatToIntStr(id), updateBody)
	if rec.Code != http.StatusOK {
		t.Errorf("PUT /api/monitors/:id returned %d; want 200", rec.Code)
	}

	var updated map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if updated["name"] != "Updated Monitor" {
		t.Errorf("name = %v; want Updated Monitor", updated["name"])
	}

	// Effect: re-fetch with a fresh GET — not the PUT response body — to prove
	// the rename was actually persisted rather than merely echoed back. The
	// handler builds its PUT response from the same in-memory struct it hands
	// to svc.Update, so a persistence bug would still echo the new name here.
	getRec := h.do(t, http.MethodGet, "/api/monitors/"+floatToIntStr(id), nil)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET after update returned %d; want 200", getRec.Code)
	}
	var fetched map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode post-update get response: %v", err)
	}
	if fetched["name"] != "Updated Monitor" {
		t.Errorf("GET after update: name = %v; want Updated Monitor (PUT returned 200 but did not persist)", fetched["name"])
	}
}

func TestMonitorHandlers_Delete(t *testing.T) {
	h := newMonitorHarness(t)

	// Create.
	body := map[string]any{
		"name":   "To Delete",
		"type":   "http",
		"config": map[string]any{"url": "https://example.com"},
	}
	createRec := h.do(t, http.MethodPost, "/api/monitors", body)
	var created map[string]any
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	id := created["id"].(float64)

	// Delete.
	rec := h.do(t, http.MethodDelete, "/api/monitors/"+floatToIntStr(id), nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("DELETE /api/monitors/:id returned %d; want 204", rec.Code)
	}

	// Verify it's gone.
	getRec := h.do(t, http.MethodGet, "/api/monitors/"+floatToIntStr(id), nil)
	if getRec.Code != http.StatusNotFound {
		t.Errorf("GET after delete returned %d; want 404", getRec.Code)
	}
}

// TestMonitorHandlers_Update_ClearsGroupID proves that PUT .../:id with
// "group_id": null actually clears a monitor's group — a bare 200 response
// proves nothing, since a handler that silently ignored the field would
// return 200 too. We seed the monitor's GroupID directly on the fake repo
// (bypassing group-ownership validation, which belongs to
// monitor_group_test.go) so the only thing under test here is whether
// MonitorHandlers.Update actually applies req.GroupID — including when it's
// nil — rather than gating it on non-zero like a normal partial-update field.
func TestMonitorHandlers_Update_ClearsGroupID(t *testing.T) {
	h := newMonitorHarness(t)

	body := map[string]any{
		"name":   "Grouped Monitor",
		"type":   "http",
		"config": map[string]any{"url": "https://example.com"},
	}
	createRec := h.do(t, http.MethodPost, "/api/monitors", body)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: %d", createRec.Code)
	}
	var created map[string]any
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	id := created["id"].(float64)

	// Seed an existing group assignment directly on the repo.
	groupID := int64(999)
	h.repo.mu.Lock()
	h.repo.byID[int64(id)].GroupID = &groupID
	h.repo.mu.Unlock()

	// Sanity check the seed actually took, so the eventual nil assertion
	// below is meaningful rather than trivially true.
	seedRec := h.do(t, http.MethodGet, "/api/monitors/"+floatToIntStr(id), nil)
	var seeded map[string]any
	if err := json.Unmarshal(seedRec.Body.Bytes(), &seeded); err != nil {
		t.Fatalf("decode seed-check response: %v", err)
	}
	if seeded["group_id"] == nil {
		t.Fatalf("test setup: group_id should be non-nil before the clearing PUT")
	}

	// PUT with group_id explicitly null must clear it.
	updateBody := map[string]any{
		"name":     "Grouped Monitor",
		"group_id": nil,
	}
	rec := h.do(t, http.MethodPut, "/api/monitors/"+floatToIntStr(id), updateBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/monitors/:id returned %d; want 200", rec.Code)
	}

	// Re-fetch with a fresh GET — not the PUT response body — to prove the
	// clear was actually persisted rather than merely echoed back.
	getRec := h.do(t, http.MethodGet, "/api/monitors/"+floatToIntStr(id), nil)
	var after map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode post-clear get response: %v", err)
	}
	if after["group_id"] != nil {
		t.Errorf("group_id after clearing PUT = %v; want nil", after["group_id"])
	}
}

// TestMonitorHandlers_TLSIgnoreRoundTrip asserts the tls_ignore wire contract
// (top-level boolean on create/update/view) AND its effect: the flag must land
// on the persisted domain.Monitor, because that is what the scheduler reads
// when it builds the checker config. Serializing it back alone would be a
// green-gate illusion.
func TestMonitorHandlers_TLSIgnoreRoundTrip(t *testing.T) {
	h := newMonitorHarness(t)

	body := map[string]any{
		"name":       "TLS Ignore Monitor",
		"type":       "http",
		"config":     map[string]any{"url": "https://self-signed.internal"},
		"tls_ignore": true,
	}
	createRec := h.do(t, http.MethodPost, "/api/monitors", body)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: %d", createRec.Code)
	}
	var created map[string]any
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created["tls_ignore"] != true {
		t.Errorf("create response tls_ignore = %v; want true", created["tls_ignore"])
	}
	id := created["id"].(float64)

	// Effect: the persisted monitor carries the flag.
	stored, err := h.repo.GetByID(context.Background(), int64(id))
	if err != nil {
		t.Fatalf("repo GetByID: %v", err)
	}
	if !stored.TLSIgnore {
		t.Error("persisted monitor TLSIgnore = false; want true")
	}

	getRec := h.do(t, http.MethodGet, "/api/monitors/"+floatToIntStr(id), nil)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get: %d", getRec.Code)
	}
	var fetched map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if fetched["tls_ignore"] != true {
		t.Errorf("GET tls_ignore = %v; want true", fetched["tls_ignore"])
	}

	// tls_ignore is always applied on update (UpsideDown semantics): sending
	// false must flip the stored flag off again.
	updRec := h.do(t, http.MethodPut, "/api/monitors/"+floatToIntStr(id), map[string]any{"tls_ignore": false})
	if updRec.Code != http.StatusOK {
		t.Fatalf("update: %d", updRec.Code)
	}
	var updated map[string]any
	if err := json.Unmarshal(updRec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if updated["tls_ignore"] != false {
		t.Errorf("PUT tls_ignore = %v; want false", updated["tls_ignore"])
	}
	stored, err = h.repo.GetByID(context.Background(), int64(id))
	if err != nil {
		t.Fatalf("repo GetByID after update: %v", err)
	}
	if stored.TLSIgnore {
		t.Error("persisted monitor TLSIgnore = true after clearing PUT; want false")
	}
}

func floatToIntStr(f float64) string {
	return intToStr(int64(f))
}

func intToStr(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
