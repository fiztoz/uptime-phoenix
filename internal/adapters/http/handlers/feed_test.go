package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// --- fakes scoped to feed handler tests ------------------------------------

type feedFakeSPRepo struct {
	mu   sync.Mutex
	byID map[int64]*domain.StatusPage
	next int64
}

func newFeedFakeSPRepo() *feedFakeSPRepo {
	return &feedFakeSPRepo{byID: make(map[int64]*domain.StatusPage)}
}

func (r *feedFakeSPRepo) Create(_ context.Context, sp *domain.StatusPage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	sp.ID = r.next
	cp := *sp
	r.byID[sp.ID] = &cp
	return nil
}
func (r *feedFakeSPRepo) GetByID(_ context.Context, id int64) (*domain.StatusPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sp, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *sp
	return &cp, nil
}
func (r *feedFakeSPRepo) GetBySlug(_ context.Context, slug string) (*domain.StatusPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, sp := range r.byID {
		if sp.Slug == slug {
			cp := *sp
			return &cp, nil
		}
	}
	return nil, ports.ErrNotFound
}
func (r *feedFakeSPRepo) List(context.Context) ([]*domain.StatusPage, error) { return nil, nil }
func (r *feedFakeSPRepo) Update(context.Context, *domain.StatusPage) error   { return nil }
func (r *feedFakeSPRepo) Delete(context.Context, int64) error                { return nil }

type feedFakeIncidentRepo struct {
	mu   sync.Mutex
	byID map[int64]*domain.Incident
	next int64
}

func newFeedFakeIncidentRepo() *feedFakeIncidentRepo {
	return &feedFakeIncidentRepo{byID: make(map[int64]*domain.Incident)}
}

func (r *feedFakeIncidentRepo) Create(_ context.Context, inc *domain.Incident) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	inc.ID = r.next
	if inc.CreatedAt.IsZero() {
		inc.CreatedAt = time.Now().UTC()
	}
	cp := *inc
	r.byID[inc.ID] = &cp
	return nil
}
func (r *feedFakeIncidentRepo) GetByID(context.Context, int64) (*domain.Incident, error) {
	return nil, ports.ErrNotFound
}
func (r *feedFakeIncidentRepo) ListByStatusPage(_ context.Context, statusPageID int64) ([]*domain.Incident, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.Incident
	for _, inc := range r.byID {
		if inc.StatusPageID == statusPageID {
			cp := *inc
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (r *feedFakeIncidentRepo) ListAll(context.Context) ([]*domain.Incident, error) { return nil, nil }
func (r *feedFakeIncidentRepo) Update(context.Context, *domain.Incident) error      { return nil }
func (r *feedFakeIncidentRepo) Delete(context.Context, int64) error                 { return nil }

type feedFakeSPMonitorRepo struct {
	mu    sync.Mutex
	links []*domain.StatusPageMonitor
	next  int64
}

func newFeedFakeSPMonitorRepo() *feedFakeSPMonitorRepo {
	return &feedFakeSPMonitorRepo{}
}

func (r *feedFakeSPMonitorRepo) AddMonitor(_ context.Context, spID, monitorID int64, displayOrder int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	r.links = append(r.links, &domain.StatusPageMonitor{
		ID: r.next, StatusPageID: spID, MonitorID: monitorID, DisplayOrder: displayOrder,
	})
	return nil
}
func (r *feedFakeSPMonitorRepo) RemoveMonitor(context.Context, int64, int64) error { return nil }
func (r *feedFakeSPMonitorRepo) ReorderMonitors(context.Context, int64, []int64) error {
	return nil
}
func (r *feedFakeSPMonitorRepo) ListByStatusPage(_ context.Context, statusPageID int64) ([]*domain.StatusPageMonitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.StatusPageMonitor
	for _, l := range r.links {
		if l.StatusPageID == statusPageID {
			cp := *l
			out = append(out, &cp)
		}
	}
	return out, nil
}

type feedPasswordHasher struct{}

func (feedPasswordHasher) Hash(password string) (string, error) { return "hashed:" + password, nil }
func (feedPasswordHasher) Verify(hashed, password string) error {
	if hashed != "hashed:"+password {
		return ports.ErrNotFound // any non-nil error
	}
	return nil
}

type feedFakeMaintRepo struct {
	windows map[int64]*domain.MaintenanceWindow
}

func (r *feedFakeMaintRepo) Create(context.Context, *domain.MaintenanceWindow) error { return nil }
func (r *feedFakeMaintRepo) GetByID(_ context.Context, id int64) (*domain.MaintenanceWindow, error) {
	w, ok := r.windows[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *w
	return &cp, nil
}
func (r *feedFakeMaintRepo) List(context.Context, int64) ([]*domain.MaintenanceWindow, error) {
	return nil, nil
}
func (r *feedFakeMaintRepo) ListAll(context.Context) ([]*domain.MaintenanceWindow, error) {
	return nil, nil
}
func (r *feedFakeMaintRepo) Update(context.Context, *domain.MaintenanceWindow) error { return nil }
func (r *feedFakeMaintRepo) Delete(context.Context, int64) error                     { return nil }

type feedFakeMaintLinkRepo struct {
	byMonitor map[int64][]*domain.MaintenanceWindow
}

func (r *feedFakeMaintLinkRepo) Assign(context.Context, int64, int64) error { return nil }
func (r *feedFakeMaintLinkRepo) Remove(context.Context, int64, int64) error { return nil }
func (r *feedFakeMaintLinkRepo) ListByMaintenance(context.Context, int64) ([]int64, error) {
	return nil, nil
}
func (r *feedFakeMaintLinkRepo) ListByMonitor(_ context.Context, monitorID int64) ([]*domain.MaintenanceWindow, error) {
	return r.byMonitor[monitorID], nil
}

type feedFakeCronEval struct{}

func (feedFakeCronEval) IsWindowActive(string, int, time.Time, *time.Location) bool { return false }

func feedHandlersWith(
	sp *feedFakeSPRepo,
	inc *feedFakeIncidentRepo,
	spMon *feedFakeSPMonitorRepo,
	maint *services.MaintenanceService,
	hasher ports.PasswordHasher,
) *FeedHandlers {
	svc := services.NewFeedService(sp, inc, spMon, maint, hasher, "https://status.example")
	return NewFeedHandlers(svc)
}

func feedEchoContext(method, path, slug string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/api/status/:slug/feed.xml")
	c.SetParamNames("slug")
	c.SetParamValues(slug)
	return c, rec
}

func TestFeedAtom_UnknownSlugNotFound(t *testing.T) {
	h := feedHandlersWith(newFeedFakeSPRepo(), newFeedFakeIncidentRepo(), newFeedFakeSPMonitorRepo(), nil, nil)
	c, rec := feedEchoContext(http.MethodGet, "/api/status/missing/feed.xml", "missing")
	if err := h.Atom(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestFeedAtom_ProtectedWithoutAccessForbidden(t *testing.T) {
	ctx := context.Background()
	spRepo := newFeedFakeSPRepo()
	incRepo := newFeedFakeIncidentRepo()
	hasher := feedPasswordHasher{}
	hash, _ := hasher.Hash("secret-code-ok")
	sp := &domain.StatusPage{
		Slug: "locked", Title: "Locked", Published: true, PasswordHash: hash,
	}
	_ = spRepo.Create(ctx, sp)
	_ = incRepo.Create(ctx, &domain.Incident{
		StatusPageID: sp.ID, Title: "Hidden incident", Active: true,
	})

	h := feedHandlersWith(spRepo, incRepo, newFeedFakeSPMonitorRepo(), nil, hasher)
	c, rec := feedEchoContext(http.MethodGet, "/api/status/locked/feed.xml", "locked")
	if err := h.Atom(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "access denied" {
		t.Fatalf("body = %v", body)
	}
	if strings.Contains(rec.Body.String(), hash) || strings.Contains(rec.Body.String(), "PasswordHash") {
		t.Fatal("response leaked password hash")
	}
}

func TestFeedAtom_IncludesActiveIncidentTitleNoPasswordHash(t *testing.T) {
	ctx := context.Background()
	spRepo := newFeedFakeSPRepo()
	incRepo := newFeedFakeIncidentRepo()
	sp := &domain.StatusPage{Slug: "acme", Title: "Acme Status", Published: true}
	_ = spRepo.Create(ctx, sp)
	_ = incRepo.Create(ctx, &domain.Incident{
		StatusPageID: sp.ID,
		Title:        "API elevated errors",
		Content:      "Investigating latency spike",
		Style:        "danger",
		Active:       true,
		CreatedAt:    time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
	})

	h := feedHandlersWith(spRepo, incRepo, newFeedFakeSPMonitorRepo(), nil, nil)
	c, rec := feedEchoContext(http.MethodGet, "/api/status/acme/feed.xml", "acme")
	if err := h.Atom(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get(echo.HeaderContentType)
	if !strings.Contains(ct, "application/atom+xml") {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "API elevated errors") {
		t.Fatalf("feed missing incident title:\n%s", body)
	}
	if !strings.Contains(body, `xmlns="http://www.w3.org/2005/Atom"`) &&
		!strings.Contains(body, "http://www.w3.org/2005/Atom") {
		t.Fatalf("not atom:\n%s", body)
	}
	// Secrets and domain field names must never appear.
	for _, leak := range []string{"PasswordHash", "password_hash", "hashed:", "bcrypt"} {
		if strings.Contains(body, leak) {
			t.Fatalf("feed leaked %q:\n%s", leak, body)
		}
	}
}

func TestFeedAtom_AccessCodeQueryUnlocks(t *testing.T) {
	ctx := context.Background()
	spRepo := newFeedFakeSPRepo()
	incRepo := newFeedFakeIncidentRepo()
	hasher := feedPasswordHasher{}
	hash, _ := hasher.Hash("opensesame")
	sp := &domain.StatusPage{
		Slug: "gate", Title: "Gated", Published: true, PasswordHash: hash,
	}
	_ = spRepo.Create(ctx, sp)
	_ = incRepo.Create(ctx, &domain.Incident{
		StatusPageID: sp.ID, Title: "Private outage", Active: true,
	})

	h := feedHandlersWith(spRepo, incRepo, newFeedFakeSPMonitorRepo(), nil, hasher)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/status/gate/feed.xml?access_code=opensesame", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/api/status/:slug/feed.xml")
	c.SetParamNames("slug")
	c.SetParamValues("gate")
	if err := h.Atom(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Private outage") {
		t.Fatalf("missing title: %s", rec.Body.String())
	}
}

func TestFeedCalendar_IncludesMaintenanceVEVENTSummary(t *testing.T) {
	ctx := context.Background()
	spRepo := newFeedFakeSPRepo()
	spMon := newFeedFakeSPMonitorRepo()
	sp := &domain.StatusPage{Slug: "acme", Title: "Acme Status", Published: true}
	_ = spRepo.Create(ctx, sp)
	_ = spMon.AddMonitor(ctx, sp.ID, 99, 10)

	start := time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Minute)
	mw := &domain.MaintenanceWindow{
		ID: 3, Title: "Cache cluster restart", Description: "Rolling restart",
		Active: true, Strategy: "single", StartDate: start, EndDate: end, Timezone: "UTC",
	}
	maintRepo := &feedFakeMaintRepo{windows: map[int64]*domain.MaintenanceWindow{3: mw}}
	linkRepo := &feedFakeMaintLinkRepo{byMonitor: map[int64][]*domain.MaintenanceWindow{99: {mw}}}
	maintSvc := services.NewMaintenanceService(maintRepo, linkRepo, feedFakeCronEval{})

	h := feedHandlersWith(spRepo, newFeedFakeIncidentRepo(), spMon, maintSvc, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/status/acme/calendar.ics", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/api/status/:slug/calendar.ics")
	c.SetParamNames("slug")
	c.SetParamValues("acme")

	if err := h.Calendar(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get(echo.HeaderContentType)
	if !strings.Contains(ct, "text/calendar") {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "BEGIN:VEVENT") {
		t.Fatalf("missing VEVENT:\n%s", body)
	}
	if !strings.Contains(body, "SUMMARY:Cache cluster restart") {
		t.Fatalf("missing summary:\n%s", body)
	}
	if !strings.Contains(body, "DTSTART:20260801T010000Z") {
		t.Fatalf("missing DTSTART:\n%s", body)
	}
	if strings.Contains(body, "PasswordHash") {
		t.Fatal("ical leaked PasswordHash")
	}
}

func TestFeedCalendar_UnknownSlugNotFound(t *testing.T) {
	h := feedHandlersWith(newFeedFakeSPRepo(), newFeedFakeIncidentRepo(), newFeedFakeSPMonitorRepo(), nil, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/status/nope/calendar.ics", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/api/status/:slug/calendar.ics")
	c.SetParamNames("slug")
	c.SetParamValues("nope")
	if err := h.Calendar(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}
