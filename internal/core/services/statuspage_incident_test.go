package services

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// --- fakes for ResolveIncident + AutoResolveOnRecovery --------------------

type fakeIncidentRepo struct {
	mu   sync.Mutex
	byID map[int64]*domain.Incident
	next int64
}

func newFakeIncidentRepo() *fakeIncidentRepo {
	return &fakeIncidentRepo{byID: make(map[int64]*domain.Incident)}
}

func (r *fakeIncidentRepo) Create(_ context.Context, inc *domain.Incident) error {
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

func (r *fakeIncidentRepo) GetByID(_ context.Context, id int64) (*domain.Incident, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inc, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *inc
	return &cp, nil
}

func (r *fakeIncidentRepo) ListByStatusPage(_ context.Context, statusPageID int64) ([]*domain.Incident, error) {
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

func (r *fakeIncidentRepo) ListAll(_ context.Context) ([]*domain.Incident, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Incident, 0, len(r.byID))
	for _, inc := range r.byID {
		cp := *inc
		out = append(out, &cp)
	}
	return out, nil
}

func (r *fakeIncidentRepo) Update(_ context.Context, inc *domain.Incident) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[inc.ID]; !ok {
		return ports.ErrNotFound
	}
	cp := *inc
	r.byID[inc.ID] = &cp
	return nil
}

func (r *fakeIncidentRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}

type fakeIncidentUpdateRepo struct {
	mu   sync.Mutex
	byID map[int64]*domain.IncidentUpdate
	next int64
}

func newFakeIncidentUpdateRepo() *fakeIncidentUpdateRepo {
	return &fakeIncidentUpdateRepo{byID: make(map[int64]*domain.IncidentUpdate)}
}

func (r *fakeIncidentUpdateRepo) Create(_ context.Context, update *domain.IncidentUpdate) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	update.ID = r.next
	if update.CreatedAt.IsZero() {
		update.CreatedAt = time.Now().UTC().Add(time.Duration(r.next) * time.Second)
	}
	cp := *update
	r.byID[update.ID] = &cp
	return nil
}

func (r *fakeIncidentUpdateRepo) ListByIncident(_ context.Context, incidentID int64) ([]*domain.IncidentUpdate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.IncidentUpdate
	for _, update := range r.byID {
		if update.IncidentID == incidentID {
			cp := *update
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *fakeIncidentUpdateRepo) ListByStatusPage(_ context.Context, statusPageID int64) ([]*domain.IncidentUpdate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.IncidentUpdate
	for _, update := range r.byID {
		if update.StatusPageID == statusPageID {
			cp := *update
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

type fakeSPRepo struct {
	mu   sync.Mutex
	byID map[int64]*domain.StatusPage
	next int64
}

func newFakeSPRepo() *fakeSPRepo {
	return &fakeSPRepo{byID: make(map[int64]*domain.StatusPage)}
}

func (r *fakeSPRepo) Create(_ context.Context, sp *domain.StatusPage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	sp.ID = r.next
	cp := *sp
	r.byID[sp.ID] = &cp
	return nil
}

func (r *fakeSPRepo) GetByID(_ context.Context, id int64) (*domain.StatusPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sp, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *sp
	return &cp, nil
}

func (r *fakeSPRepo) GetBySlug(_ context.Context, slug string) (*domain.StatusPage, error) {
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

func (r *fakeSPRepo) List(_ context.Context) ([]*domain.StatusPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.StatusPage, 0, len(r.byID))
	for _, sp := range r.byID {
		cp := *sp
		out = append(out, &cp)
	}
	return out, nil
}

func (r *fakeSPRepo) Update(_ context.Context, sp *domain.StatusPage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *sp
	r.byID[sp.ID] = &cp
	return nil
}

func (r *fakeSPRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}

type fakeSPMonitorRepo struct {
	mu    sync.Mutex
	links []*domain.StatusPageMonitor
	next  int64
}

func newFakeSPMonitorRepo() *fakeSPMonitorRepo {
	return &fakeSPMonitorRepo{}
}

func (r *fakeSPMonitorRepo) AddMonitor(_ context.Context, spID, monitorID int64, displayOrder int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	r.links = append(r.links, &domain.StatusPageMonitor{
		ID: r.next, StatusPageID: spID, MonitorID: monitorID, DisplayOrder: displayOrder,
	})
	return nil
}

func (r *fakeSPMonitorRepo) RemoveMonitor(_ context.Context, spID, monitorID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.links[:0]
	for _, l := range r.links {
		if !(l.StatusPageID == spID && l.MonitorID == monitorID) {
			out = append(out, l)
		}
	}
	r.links = out
	return nil
}

func (r *fakeSPMonitorRepo) ReorderMonitors(_ context.Context, spID int64, monitorIDs []int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	filtered := r.links[:0]
	for _, l := range r.links {
		if l.StatusPageID != spID {
			filtered = append(filtered, l)
		}
	}
	r.links = filtered
	for i, mid := range monitorIDs {
		r.next++
		r.links = append(r.links, &domain.StatusPageMonitor{
			ID: r.next, StatusPageID: spID, MonitorID: mid, DisplayOrder: (i + 1) * 10,
		})
	}
	return nil
}

func (r *fakeSPMonitorRepo) ListByStatusPage(_ context.Context, statusPageID int64) ([]*domain.StatusPageMonitor, error) {
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

func newSPServiceForIncidentTests(
	spRepo *fakeSPRepo,
	incRepo *fakeIncidentRepo,
	spMon *fakeSPMonitorRepo,
) *StatusPageService {
	return NewStatusPageService(spRepo, incRepo, nil, spMon, nil, nil, nil)
}

// ResolveIncident must flip Active=false and persist — that is the only
// resolution model (there is no resolved_at). Asserts the effect, not a status code.
func TestResolveIncident_SetsActiveFalse(t *testing.T) {
	spRepo := newFakeSPRepo()
	incRepo := newFakeIncidentRepo()
	spMon := newFakeSPMonitorRepo()
	svc := newSPServiceForIncidentTests(spRepo, incRepo, spMon)
	ctx := context.Background()

	sp := &domain.StatusPage{Slug: "main", Title: "Main", Published: true}
	if err := spRepo.Create(ctx, sp); err != nil {
		t.Fatalf("create sp: %v", err)
	}
	inc := &domain.Incident{
		StatusPageID: sp.ID,
		Title:        "Outage",
		Content:      "API down",
		Style:        "danger",
		Active:       true,
	}
	if err := incRepo.Create(ctx, inc); err != nil {
		t.Fatalf("create incident: %v", err)
	}

	if err := svc.ResolveIncident(ctx, inc.ID); err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}

	got, err := incRepo.GetByID(ctx, inc.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Active {
		t.Fatal("incident still Active=true after ResolveIncident — the public banner would never clear")
	}
}

func TestDeleteIncident_RejectsActiveAndDeletesResolved(t *testing.T) {
	spRepo := newFakeSPRepo()
	incRepo := newFakeIncidentRepo()
	svc := newSPServiceForIncidentTests(spRepo, incRepo, newFakeSPMonitorRepo())
	ctx := context.Background()

	sp := &domain.StatusPage{Slug: "delete", Title: "Delete", Published: true}
	if err := spRepo.Create(ctx, sp); err != nil {
		t.Fatalf("create sp: %v", err)
	}
	inc := &domain.Incident{
		StatusPageID: sp.ID,
		Title:        "API outage",
		Content:      "Investigating.",
		Style:        "danger",
		Active:       true,
	}
	if err := incRepo.Create(ctx, inc); err != nil {
		t.Fatalf("create incident: %v", err)
	}

	if err := svc.DeleteIncident(ctx, inc.ID); !errors.Is(err, domain.ErrIncidentActive) {
		t.Fatalf("DeleteIncident(active) error = %v, want ErrIncidentActive", err)
	}
	if _, err := incRepo.GetByID(ctx, inc.ID); err != nil {
		t.Fatalf("active incident was deleted: %v", err)
	}

	if err := svc.ResolveIncident(ctx, inc.ID); err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}
	if err := svc.DeleteIncident(ctx, inc.ID); err != nil {
		t.Fatalf("DeleteIncident(resolved): %v", err)
	}
	if _, err := incRepo.GetByID(ctx, inc.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("GetByID after deletion error = %v, want not found", err)
	}
}

func TestIncidentUpdates_ProgressInOrderAndResolve(t *testing.T) {
	spRepo := newFakeSPRepo()
	incRepo := newFakeIncidentRepo()
	updateRepo := newFakeIncidentUpdateRepo()
	svc := newSPServiceForIncidentTests(spRepo, incRepo, newFakeSPMonitorRepo())
	svc.SetIncidentUpdateRepo(updateRepo)
	ctx := context.Background()

	sp := &domain.StatusPage{Slug: "timeline", Title: "Timeline", Published: true}
	if err := spRepo.Create(ctx, sp); err != nil {
		t.Fatalf("create sp: %v", err)
	}
	inc := &domain.Incident{
		StatusPageID: sp.ID,
		Title:        "API outage",
		Content:      "We are investigating.",
		Style:        "danger",
		Active:       true,
	}
	if err := svc.CreateIncident(ctx, inc); err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}
	if _, err := svc.CreateIncidentUpdate(ctx, inc.ID, domain.IncidentStatusIdentified, "Root cause **identified**."); err != nil {
		t.Fatalf("identified update: %v", err)
	}
	if _, err := svc.CreateIncidentUpdate(ctx, inc.ID, domain.IncidentStatusMonitoring, "Mitigation deployed."); err != nil {
		t.Fatalf("monitoring update: %v", err)
	}
	if _, err := svc.CreateIncidentUpdate(ctx, inc.ID, domain.IncidentStatusResolved, "Resolved."); err != nil {
		t.Fatalf("resolved update: %v", err)
	}

	got, err := incRepo.GetByID(ctx, inc.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Active {
		t.Fatal("resolved update did not mark incident inactive")
	}
	updates, err := svc.ListIncidentUpdates(ctx, inc.ID)
	if err != nil {
		t.Fatalf("ListIncidentUpdates: %v", err)
	}
	if len(updates) != 4 {
		t.Fatalf("updates length = %d, want initial + 3", len(updates))
	}
	if updates[0].Status != domain.IncidentStatusInvestigating || updates[3].Status != domain.IncidentStatusResolved {
		t.Fatalf("unexpected update order: %#v", updates)
	}
}

func TestIncidentUpdates_RejectBackwardStatus(t *testing.T) {
	spRepo := newFakeSPRepo()
	incRepo := newFakeIncidentRepo()
	updateRepo := newFakeIncidentUpdateRepo()
	svc := newSPServiceForIncidentTests(spRepo, incRepo, newFakeSPMonitorRepo())
	svc.SetIncidentUpdateRepo(updateRepo)
	ctx := context.Background()

	sp := &domain.StatusPage{Slug: "timeline", Title: "Timeline", Published: true}
	_ = spRepo.Create(ctx, sp)
	inc := &domain.Incident{StatusPageID: sp.ID, Title: "API outage", Active: true}
	if err := svc.CreateIncident(ctx, inc); err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}
	if _, err := svc.CreateIncidentUpdate(ctx, inc.ID, domain.IncidentStatusMonitoring, "Watching"); err != nil {
		t.Fatalf("monitoring update: %v", err)
	}
	if _, err := svc.CreateIncidentUpdate(ctx, inc.ID, domain.IncidentStatusIdentified, "Backwards"); err == nil {
		t.Fatal("backward status update succeeded; want validation error")
	}
}

// AutoResolveOnRecovery resolves active incidents only when the status page
// has AutoResolveIncidents and includes the recovering monitor.
func TestAutoResolveOnRecovery_FlagOnResolves(t *testing.T) {
	spRepo := newFakeSPRepo()
	incRepo := newFakeIncidentRepo()
	spMon := newFakeSPMonitorRepo()
	svc := newSPServiceForIncidentTests(spRepo, incRepo, spMon)
	ctx := context.Background()

	sp := &domain.StatusPage{
		Slug: "auto", Title: "Auto", Published: true, AutoResolveIncidents: true,
	}
	if err := spRepo.Create(ctx, sp); err != nil {
		t.Fatalf("create sp: %v", err)
	}
	const monitorID int64 = 42
	if err := spMon.AddMonitor(ctx, sp.ID, monitorID, 1); err != nil {
		t.Fatalf("link monitor: %v", err)
	}
	inc := &domain.Incident{
		StatusPageID: sp.ID, Title: "Outage", Style: "danger", Active: true,
	}
	if err := incRepo.Create(ctx, inc); err != nil {
		t.Fatalf("create incident: %v", err)
	}

	if err := svc.AutoResolveOnRecovery(ctx, monitorID); err != nil {
		t.Fatalf("AutoResolveOnRecovery: %v", err)
	}

	got, _ := incRepo.GetByID(ctx, inc.ID)
	if got.Active {
		t.Fatal("incident still active after recovery with auto_resolve_incidents=true")
	}
}

func TestAutoResolveOnRecovery_FlagOffLeavesActive(t *testing.T) {
	spRepo := newFakeSPRepo()
	incRepo := newFakeIncidentRepo()
	spMon := newFakeSPMonitorRepo()
	svc := newSPServiceForIncidentTests(spRepo, incRepo, spMon)
	ctx := context.Background()

	sp := &domain.StatusPage{
		Slug: "manual", Title: "Manual", Published: true, AutoResolveIncidents: false,
	}
	if err := spRepo.Create(ctx, sp); err != nil {
		t.Fatalf("create sp: %v", err)
	}
	const monitorID int64 = 7
	_ = spMon.AddMonitor(ctx, sp.ID, monitorID, 1)
	inc := &domain.Incident{
		StatusPageID: sp.ID, Title: "Outage", Style: "danger", Active: true,
	}
	_ = incRepo.Create(ctx, inc)

	if err := svc.AutoResolveOnRecovery(ctx, monitorID); err != nil {
		t.Fatalf("AutoResolveOnRecovery: %v", err)
	}

	got, _ := incRepo.GetByID(ctx, inc.ID)
	if !got.Active {
		t.Fatal("incident was resolved despite auto_resolve_incidents=false")
	}
}

func TestAutoResolveOnRecovery_UnrelatedMonitorLeavesActive(t *testing.T) {
	spRepo := newFakeSPRepo()
	incRepo := newFakeIncidentRepo()
	spMon := newFakeSPMonitorRepo()
	svc := newSPServiceForIncidentTests(spRepo, incRepo, spMon)
	ctx := context.Background()

	sp := &domain.StatusPage{
		Slug: "auto2", Title: "Auto2", Published: true, AutoResolveIncidents: true,
	}
	_ = spRepo.Create(ctx, sp)
	_ = spMon.AddMonitor(ctx, sp.ID, 1, 1) // only monitor 1 is linked
	inc := &domain.Incident{
		StatusPageID: sp.ID, Title: "Outage", Style: "danger", Active: true,
	}
	_ = incRepo.Create(ctx, inc)

	// Monitor 99 recovers — not on this page.
	if err := svc.AutoResolveOnRecovery(ctx, 99); err != nil {
		t.Fatalf("AutoResolveOnRecovery: %v", err)
	}
	got, _ := incRepo.GetByID(ctx, inc.ID)
	if !got.Active {
		t.Fatal("incident resolved for an unrelated monitor recovery")
	}
}
