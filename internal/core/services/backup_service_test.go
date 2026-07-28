package services

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// --- fakes for backup export/import ---------------------------------------

type backupFakeMonitorRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.Monitor
	nextID int64
}

func newBackupFakeMonitorRepo() *backupFakeMonitorRepo {
	return &backupFakeMonitorRepo{byID: make(map[int64]*domain.Monitor)}
}

func (r *backupFakeMonitorRepo) Create(_ context.Context, m *domain.Monitor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	m.ID = r.nextID
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = m.CreatedAt
	}
	cp := *m
	if m.Config != nil {
		cp.Config = copyMap(m.Config)
	}
	r.byID[m.ID] = &cp
	return nil
}

func (r *backupFakeMonitorRepo) GetByID(_ context.Context, id int64) (*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *m
	return &cp, nil
}

func (r *backupFakeMonitorRepo) GetByPushToken(_ context.Context, _ string) (*domain.Monitor, error) {
	return nil, ports.ErrNotFound
}

func (r *backupFakeMonitorRepo) List(_ context.Context, filter ports.MonitorFilter) ([]*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Monitor, 0)
	for _, m := range r.byID {
		if filter.UserID > 0 && m.UserID != filter.UserID {
			continue
		}
		cp := *m
		out = append(out, &cp)
	}
	return out, nil
}

func (r *backupFakeMonitorRepo) ListActive(_ context.Context) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *backupFakeMonitorRepo) Update(_ context.Context, m *domain.Monitor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[m.ID]; !ok {
		return ports.ErrNotFound
	}
	cp := *m
	r.byID[m.ID] = &cp
	return nil
}
func (r *backupFakeMonitorRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}
func (r *backupFakeMonitorRepo) ClaimBatch(_ context.Context, _ string, _ int, _ time.Duration) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *backupFakeMonitorRepo) RefreshLease(_ context.Context, _ string) (int64, error) {
	return 0, nil
}
func (r *backupFakeMonitorRepo) ReleaseLeases(_ context.Context, _ string) (int64, error) {
	return 0, nil
}

type backupFakeGroupRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.MonitorGroup
	nextID int64
}

func newBackupFakeGroupRepo() *backupFakeGroupRepo {
	return &backupFakeGroupRepo{byID: make(map[int64]*domain.MonitorGroup)}
}

func (r *backupFakeGroupRepo) Create(_ context.Context, g *domain.MonitorGroup) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	g.ID = r.nextID
	cp := *g
	r.byID[g.ID] = &cp
	return nil
}
func (r *backupFakeGroupRepo) GetByID(_ context.Context, id int64) (*domain.MonitorGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *g
	return &cp, nil
}
func (r *backupFakeGroupRepo) List(_ context.Context, userID int64) ([]*domain.MonitorGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.MonitorGroup, 0)
	for _, g := range r.byID {
		if userID > 0 && g.UserID != userID {
			continue
		}
		cp := *g
		out = append(out, &cp)
	}
	return out, nil
}
func (r *backupFakeGroupRepo) ListAll(ctx context.Context) ([]*domain.MonitorGroup, error) {
	return r.List(ctx, 0)
}
func (r *backupFakeGroupRepo) Update(_ context.Context, g *domain.MonitorGroup) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.byID[g.ID]
	if !ok {
		return ports.ErrNotFound
	}
	cp := *g
	cp.LastStatus = existing.LastStatus // owned by ClaimStatusTransition — see grpFakeGroupRepo
	r.byID[g.ID] = &cp
	return nil
}

// ClaimStatusTransition — see grpFakeGroupRepo.ClaimStatusTransition. Compare-and-set,
// not an unconditional write.
func (r *backupFakeGroupRepo) ClaimStatusTransition(_ context.Context, groupID int64, from *domain.Status, to domain.Status) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.byID[groupID]
	if !ok {
		return false, ports.ErrNotFound
	}
	if !statusPtrEqual(g.LastStatus, from) {
		return false, nil
	}
	next := to
	g.LastStatus = &next
	return true, nil
}
func (r *backupFakeGroupRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}

type backupFakeNotifRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.Notification
	nextID int64
}

func newBackupFakeNotifRepo() *backupFakeNotifRepo {
	return &backupFakeNotifRepo{byID: make(map[int64]*domain.Notification)}
}

func (r *backupFakeNotifRepo) Create(_ context.Context, n *domain.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	n.ID = r.nextID
	cp := *n
	if n.Config != nil {
		cp.Config = copyMap(n.Config)
	}
	r.byID[n.ID] = &cp
	return nil
}
func (r *backupFakeNotifRepo) GetByID(_ context.Context, id int64) (*domain.Notification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *n
	return &cp, nil
}
func (r *backupFakeNotifRepo) GetByMonitorID(_ context.Context, _ int64) ([]*domain.Notification, error) {
	return nil, nil
}
func (r *backupFakeNotifRepo) List(_ context.Context, userID int64) ([]*domain.Notification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Notification, 0)
	for _, n := range r.byID {
		if userID > 0 && n.UserID != userID {
			continue
		}
		cp := *n
		out = append(out, &cp)
	}
	return out, nil
}
func (r *backupFakeNotifRepo) ListAll(ctx context.Context) ([]*domain.Notification, error) {
	return r.List(ctx, 0)
}
func (r *backupFakeNotifRepo) Update(_ context.Context, n *domain.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *n
	r.byID[n.ID] = &cp
	return nil
}
func (r *backupFakeNotifRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}

type backupFakeMonitorNotifRepo struct {
	mu    sync.Mutex
	links []domain.MonitorNotification
}

func (r *backupFakeMonitorNotifRepo) Attach(_ context.Context, monitorID, notificationID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.links = append(r.links, domain.MonitorNotification{MonitorID: monitorID, NotificationID: notificationID})
	return nil
}
func (r *backupFakeMonitorNotifRepo) Detach(_ context.Context, _, _ int64) error { return nil }
func (r *backupFakeMonitorNotifRepo) ListByMonitor(_ context.Context, monitorID int64) ([]*domain.MonitorNotification, error) {
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
func (r *backupFakeMonitorNotifRepo) ListByNotification(_ context.Context, notificationID int64) ([]*domain.MonitorNotification, error) {
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

type backupFakeTagRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.Tag
	nextID int64
}

func newBackupFakeTagRepo() *backupFakeTagRepo {
	return &backupFakeTagRepo{byID: make(map[int64]*domain.Tag)}
}

func (r *backupFakeTagRepo) Create(_ context.Context, t *domain.Tag) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	t.ID = r.nextID
	cp := *t
	r.byID[t.ID] = &cp
	return nil
}
func (r *backupFakeTagRepo) GetByID(_ context.Context, id int64) (*domain.Tag, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *t
	return &cp, nil
}
func (r *backupFakeTagRepo) List(_ context.Context) ([]*domain.Tag, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Tag, 0, len(r.byID))
	for _, t := range r.byID {
		cp := *t
		out = append(out, &cp)
	}
	return out, nil
}
func (r *backupFakeTagRepo) Update(_ context.Context, t *domain.Tag) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *t
	r.byID[t.ID] = &cp
	return nil
}
func (r *backupFakeTagRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}

type backupFakeMonitorTagRepo struct {
	mu    sync.Mutex
	links []domain.MonitorTag
}

func (r *backupFakeMonitorTagRepo) Assign(_ context.Context, monitorID, tagID int64, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.links = append(r.links, domain.MonitorTag{MonitorID: monitorID, TagID: tagID, Value: value})
	return nil
}
func (r *backupFakeMonitorTagRepo) Remove(_ context.Context, _, _ int64) error { return nil }
func (r *backupFakeMonitorTagRepo) ListByMonitors(ctx context.Context, monitorIDs []int64) (map[int64][]*domain.MonitorTag, error) {
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
func (r *backupFakeMonitorTagRepo) ListByMonitor(_ context.Context, monitorID int64) ([]*domain.MonitorTag, error) {
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

type backupFakeStatusPageRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.StatusPage
	bySlug map[string]int64
	nextID int64
}

func newBackupFakeStatusPageRepo() *backupFakeStatusPageRepo {
	return &backupFakeStatusPageRepo{
		byID:   make(map[int64]*domain.StatusPage),
		bySlug: make(map[string]int64),
	}
}

func (r *backupFakeStatusPageRepo) Create(_ context.Context, sp *domain.StatusPage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.bySlug[sp.Slug]; ok {
		return domain.ErrConflict
	}
	r.nextID++
	sp.ID = r.nextID
	cp := *sp
	r.byID[sp.ID] = &cp
	r.bySlug[sp.Slug] = sp.ID
	return nil
}
func (r *backupFakeStatusPageRepo) GetByID(_ context.Context, id int64) (*domain.StatusPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sp, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *sp
	return &cp, nil
}
func (r *backupFakeStatusPageRepo) GetBySlug(_ context.Context, slug string) (*domain.StatusPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.bySlug[slug]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *r.byID[id]
	return &cp, nil
}
func (r *backupFakeStatusPageRepo) List(_ context.Context) ([]*domain.StatusPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.StatusPage, 0, len(r.byID))
	for _, sp := range r.byID {
		cp := *sp
		out = append(out, &cp)
	}
	return out, nil
}
func (r *backupFakeStatusPageRepo) Update(_ context.Context, sp *domain.StatusPage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *sp
	r.byID[sp.ID] = &cp
	r.bySlug[sp.Slug] = sp.ID
	return nil
}
func (r *backupFakeStatusPageRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sp, ok := r.byID[id]; ok {
		delete(r.bySlug, sp.Slug)
		delete(r.byID, id)
	}
	return nil
}

type backupFakeSPMonitorRepo struct {
	mu    sync.Mutex
	links []domain.StatusPageMonitor
}

func (r *backupFakeSPMonitorRepo) AddMonitor(_ context.Context, spID, monitorID int64, displayOrder int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.links = append(r.links, domain.StatusPageMonitor{
		StatusPageID: spID, MonitorID: monitorID, DisplayOrder: displayOrder,
	})
	return nil
}
func (r *backupFakeSPMonitorRepo) RemoveMonitor(_ context.Context, _, _ int64) error { return nil }
func (r *backupFakeSPMonitorRepo) ReorderMonitors(_ context.Context, spID int64, monitorIDs []int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.links[:0]
	for _, l := range r.links {
		if l.StatusPageID != spID {
			out = append(out, l)
		}
	}
	r.links = out
	for i, mid := range monitorIDs {
		r.links = append(r.links, domain.StatusPageMonitor{StatusPageID: spID, MonitorID: mid, DisplayOrder: (i + 1) * 10})
	}
	return nil
}
func (r *backupFakeSPMonitorRepo) ListByStatusPage(_ context.Context, statusPageID int64) ([]*domain.StatusPageMonitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.StatusPageMonitor, 0)
	for i := range r.links {
		if r.links[i].StatusPageID == statusPageID {
			cp := r.links[i]
			out = append(out, &cp)
		}
	}
	return out, nil
}

type backupFakeCNAMERepo struct {
	mu    sync.Mutex
	items []domain.StatusPageCNAME
	next  int64
}

func (r *backupFakeCNAMERepo) Create(_ context.Context, c *domain.StatusPageCNAME) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	c.ID = r.next
	r.items = append(r.items, *c)
	return nil
}
func (r *backupFakeCNAMERepo) Delete(_ context.Context, _ int64) error { return nil }
func (r *backupFakeCNAMERepo) ListByStatusPage(_ context.Context, spID int64) ([]*domain.StatusPageCNAME, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.StatusPageCNAME, 0)
	for i := range r.items {
		if r.items[i].StatusPageID == spID {
			cp := r.items[i]
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (r *backupFakeCNAMERepo) GetByDomain(_ context.Context, _ string) (*domain.StatusPageCNAME, error) {
	return nil, ports.ErrNotFound
}

type backupFakeIncidentRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.Incident
	nextID int64
}

func newBackupFakeIncidentRepo() *backupFakeIncidentRepo {
	return &backupFakeIncidentRepo{byID: make(map[int64]*domain.Incident)}
}

func (r *backupFakeIncidentRepo) Create(_ context.Context, inc *domain.Incident) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	inc.ID = r.nextID
	cp := *inc
	r.byID[inc.ID] = &cp
	return nil
}
func (r *backupFakeIncidentRepo) GetByID(_ context.Context, id int64) (*domain.Incident, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inc, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *inc
	return &cp, nil
}
func (r *backupFakeIncidentRepo) ListByStatusPage(_ context.Context, spID int64) ([]*domain.Incident, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Incident, 0)
	for _, inc := range r.byID {
		if inc.StatusPageID == spID {
			cp := *inc
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (r *backupFakeIncidentRepo) ListAll(_ context.Context) ([]*domain.Incident, error) {
	return nil, nil
}
func (r *backupFakeIncidentRepo) Update(_ context.Context, inc *domain.Incident) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *inc
	r.byID[inc.ID] = &cp
	return nil
}
func (r *backupFakeIncidentRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}

type backupFakeMaintRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.MaintenanceWindow
	nextID int64
}

func newBackupFakeMaintRepo() *backupFakeMaintRepo {
	return &backupFakeMaintRepo{byID: make(map[int64]*domain.MaintenanceWindow)}
}

func (r *backupFakeMaintRepo) Create(_ context.Context, mw *domain.MaintenanceWindow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	mw.ID = r.nextID
	cp := *mw
	r.byID[mw.ID] = &cp
	return nil
}
func (r *backupFakeMaintRepo) GetByID(_ context.Context, id int64) (*domain.MaintenanceWindow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	mw, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *mw
	return &cp, nil
}
func (r *backupFakeMaintRepo) List(_ context.Context, userID int64) ([]*domain.MaintenanceWindow, error) {
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
	return out, nil
}
func (r *backupFakeMaintRepo) ListAll(ctx context.Context) ([]*domain.MaintenanceWindow, error) {
	return r.List(ctx, 0)
}
func (r *backupFakeMaintRepo) Update(_ context.Context, mw *domain.MaintenanceWindow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *mw
	r.byID[mw.ID] = &cp
	return nil
}
func (r *backupFakeMaintRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}

type backupFakeMaintMonitorRepo struct {
	mu    sync.Mutex
	links map[int64][]int64 // maintID -> monitorIDs
}

func newBackupFakeMaintMonitorRepo() *backupFakeMaintMonitorRepo {
	return &backupFakeMaintMonitorRepo{links: make(map[int64][]int64)}
}

func (r *backupFakeMaintMonitorRepo) Assign(_ context.Context, maintenanceID, monitorID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.links[maintenanceID] = append(r.links[maintenanceID], monitorID)
	return nil
}
func (r *backupFakeMaintMonitorRepo) Remove(_ context.Context, _, _ int64) error { return nil }
func (r *backupFakeMaintMonitorRepo) ListByMaintenance(_ context.Context, maintenanceID int64) ([]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64{}, r.links[maintenanceID]...), nil
}
func (r *backupFakeMaintMonitorRepo) ListByMonitor(_ context.Context, _ int64) ([]*domain.MaintenanceWindow, error) {
	return nil, nil
}

func copyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

type backupHarness struct {
	svc           *BackupService
	monitors      *backupFakeMonitorRepo
	groups        *backupFakeGroupRepo
	notifs        *backupFakeNotifRepo
	monitorNotifs *backupFakeMonitorNotifRepo
	tags          *backupFakeTagRepo
	monitorTags   *backupFakeMonitorTagRepo
	statusPages   *backupFakeStatusPageRepo
	spMonitors    *backupFakeSPMonitorRepo
	cnames        *backupFakeCNAMERepo
	incidents     *backupFakeIncidentRepo
	maint         *backupFakeMaintRepo
	maintMonitors *backupFakeMaintMonitorRepo
	proxies       *fakeProxyRepo
}

func newBackupHarness() *backupHarness {
	h := &backupHarness{
		monitors:      newBackupFakeMonitorRepo(),
		groups:        newBackupFakeGroupRepo(),
		notifs:        newBackupFakeNotifRepo(),
		monitorNotifs: &backupFakeMonitorNotifRepo{},
		tags:          newBackupFakeTagRepo(),
		monitorTags:   &backupFakeMonitorTagRepo{},
		statusPages:   newBackupFakeStatusPageRepo(),
		spMonitors:    &backupFakeSPMonitorRepo{},
		cnames:        &backupFakeCNAMERepo{},
		incidents:     newBackupFakeIncidentRepo(),
		maint:         newBackupFakeMaintRepo(),
		maintMonitors: newBackupFakeMaintMonitorRepo(),
		proxies:       newFakeProxyRepo(),
	}
	h.svc = NewBackupService(
		h.monitors, h.groups, h.notifs, h.monitorNotifs,
		h.tags, h.monitorTags,
		h.statusPages, h.spMonitors, h.cnames, h.incidents,
		h.maint, h.maintMonitors, h.proxies,
	)
	// Prefer real service paths for proxies + monitors/groups (validation + defaults).
	h.svc.SetProxyService(NewProxyService(h.proxies))
	h.svc.SetMonitorService(NewMonitorService(h.monitors, newFakeBus()))
	h.svc.monitorSvc.SetProxyRepo(h.proxies)
	h.svc.monitorSvc.SetGroupRepo(h.groups)
	h.svc.SetMonitorGroupService(NewMonitorGroupService(h.groups, h.monitors, newFakeHeartbeatRepo(), testLogger()))
	return h
}

func TestBackupService_ExportImport_RemapsRelationships(t *testing.T) {
	ctx := context.Background()
	src := newBackupHarness()
	const userID int64 = 1

	// Seed source: proxy, notification, parent+child monitors, tag, status page, maintenance.
	proxy := &domain.Proxy{
		UserID: userID, Protocol: "http", Host: "proxy.local", Port: 8080,
		Auth: true, Username: "u", Password: "secret-proxy-pass", Active: true,
	}
	if err := src.proxies.Create(ctx, proxy); err != nil {
		t.Fatalf("create proxy: %v", err)
	}

	notif := &domain.Notification{
		UserID: userID, Name: "Telegram", Type: "telegram", Active: true,
		Config: map[string]any{"bot_token": "123:ABC", "chat_id": "-100"},
	}
	if err := src.notifs.Create(ctx, notif); err != nil {
		t.Fatalf("create notif: %v", err)
	}

	group := &domain.MonitorGroup{
		UserID: userID, Name: "Folder", Condition: domain.GroupConditionWorstOfChildren,
	}
	if err := src.groups.Create(ctx, group); err != nil {
		t.Fatalf("create group: %v", err)
	}
	sibling := &domain.Monitor{
		UserID: userID, Name: "Sibling", Type: "http", Active: true, Interval: 60, Timeout: 30,
		Config: map[string]any{"url": "https://example.com"},
	}
	if err := src.monitors.Create(ctx, sibling); err != nil {
		t.Fatalf("create sibling: %v", err)
	}
	child := &domain.Monitor{
		UserID: userID, Name: "Child", Type: "http", Active: true, Interval: 30, Timeout: 10,
		Config:  map[string]any{"url": "https://child.example.com"},
		GroupID: &group.ID, ProxyID: &proxy.ID,
		AcceptedStatusCodes: []string{"200-299", "301"},
		RetryInterval:       5, MaxRetries: 2, ResendInterval: 15, UpsideDown: true,
	}
	if err := src.monitors.Create(ctx, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	tag := &domain.Tag{Name: "prod", Color: "#ff0000"}
	if err := src.tags.Create(ctx, tag); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := src.monitorTags.Assign(ctx, child.ID, tag.ID, "primary"); err != nil {
		t.Fatalf("assign tag: %v", err)
	}
	if err := src.monitorNotifs.Attach(ctx, child.ID, notif.ID); err != nil {
		t.Fatalf("attach notif: %v", err)
	}

	slaTarget := 99.95
	sp := &domain.StatusPage{
		Slug: "public", Title: "Public Status", Theme: "dark", Published: true,
		PasswordHash: "$2a$10$fakehash", SLATarget: &slaTarget,
	}
	if err := src.statusPages.Create(ctx, sp); err != nil {
		t.Fatalf("create status page: %v", err)
	}
	if err := src.spMonitors.AddMonitor(ctx, sp.ID, child.ID, 10); err != nil {
		t.Fatalf("add sp monitor: %v", err)
	}
	if err := src.cnames.Create(ctx, &domain.StatusPageCNAME{StatusPageID: sp.ID, Domain: "status.example.com"}); err != nil {
		t.Fatalf("create cname: %v", err)
	}
	if err := src.incidents.Create(ctx, &domain.Incident{
		StatusPageID: sp.ID, Title: "Outage", Content: "oops", Style: "danger", Active: true,
	}); err != nil {
		t.Fatalf("create incident: %v", err)
	}

	mw := &domain.MaintenanceWindow{
		UserID: userID, Title: "Window", Description: "desc", Active: true,
		Strategy: "single", StartDate: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		EndDate: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	if err := src.maint.Create(ctx, mw); err != nil {
		t.Fatalf("create maint: %v", err)
	}
	if err := src.maintMonitors.Assign(ctx, mw.ID, child.ID); err != nil {
		t.Fatalf("assign maint monitor: %v", err)
	}

	doc, err := src.svc.Export(ctx, userID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if doc.Version != BackupDocumentVersion {
		t.Fatalf("version = %d, want %d", doc.Version, BackupDocumentVersion)
	}
	if len(doc.Proxies) != 1 || doc.Proxies[0].Password != "secret-proxy-pass" {
		t.Fatalf("export must include proxy password for restore, got %+v", doc.Proxies)
	}
	if len(doc.Notifications) != 1 || doc.Notifications[0].Config["bot_token"] != "123:ABC" {
		t.Fatalf("export must include notification secrets, got %+v", doc.Notifications)
	}
	if len(doc.Monitors) != 2 {
		t.Fatalf("monitors = %d, want 2", len(doc.Monitors))
	}
	if len(doc.MonitorGroups) != 1 || doc.MonitorGroups[0].Name != "Folder" {
		t.Fatalf("monitor groups export incomplete: %+v", doc.MonitorGroups)
	}
	if len(doc.Tags) != 1 || len(doc.MonitorTags) != 1 {
		t.Fatalf("tags=%d monitor_tags=%d", len(doc.Tags), len(doc.MonitorTags))
	}
	if len(doc.StatusPages) != 1 || doc.StatusPages[0].PasswordHash == "" {
		t.Fatalf("status pages export incomplete: %+v", doc.StatusPages)
	}
	if doc.StatusPages[0].SLATarget == nil || *doc.StatusPages[0].SLATarget != slaTarget {
		t.Fatalf("status page SLA target export = %v; want %v", doc.StatusPages[0].SLATarget, slaTarget)
	}

	// Import into a fresh user/store.
	dst := newBackupHarness()
	const destUser int64 = 99
	summary, err := dst.svc.Import(ctx, destUser, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.ProxiesCreated != 1 || summary.NotificationsCreated != 1 {
		t.Fatalf("summary proxies/notifs: %+v", summary)
	}
	if summary.MonitorsCreated != 2 {
		t.Fatalf("monitors created = %d, want 2; skipped=%+v", summary.MonitorsCreated, summary.Skipped)
	}
	if summary.MonitorGroupsCreated != 1 {
		t.Fatalf("monitor groups created = %d, want 1; skipped=%+v", summary.MonitorGroupsCreated, summary.Skipped)
	}
	if summary.TagsCreated != 1 || summary.MonitorTagsCreated != 1 {
		t.Fatalf("tags summary: %+v", summary)
	}
	if summary.StatusPagesCreated != 1 || summary.StatusPageMonitorsCreated != 1 {
		t.Fatalf("status page summary: %+v", summary)
	}
	if summary.MaintenanceWindowsCreated != 1 || summary.MaintenanceMonitorsCreated != 1 {
		t.Fatalf("maintenance summary: %+v", summary)
	}
	if len(summary.Skipped) != 0 {
		t.Fatalf("unexpected skips: %+v", summary.Skipped)
	}

	// Assert remapping: child still points at the imported group + proxy.
	imported, err := dst.monitors.List(ctx, ports.MonitorFilter{UserID: destUser})
	if err != nil {
		t.Fatalf("list imported: %v", err)
	}
	var impSibling, impChild *domain.Monitor
	for _, m := range imported {
		switch m.Name {
		case "Sibling":
			impSibling = m
		case "Child":
			impChild = m
		}
	}
	if impSibling == nil || impChild == nil {
		t.Fatalf("missing imported monitors: sibling=%v child=%v", impSibling, impChild)
	}
	if impSibling.GroupID != nil {
		t.Fatalf("sibling.GroupID = %v, want nil (never filed under a group)", impSibling.GroupID)
	}

	importedGroups, err := dst.groups.List(ctx, destUser)
	if err != nil {
		t.Fatalf("list imported groups: %v", err)
	}
	if len(importedGroups) != 1 || importedGroups[0].Name != "Folder" {
		t.Fatalf("imported groups = %+v, want one named Folder", importedGroups)
	}
	impGroup := importedGroups[0]
	if impChild.GroupID == nil || *impChild.GroupID != impGroup.ID {
		t.Fatalf("child.GroupID = %v, want %d", impChild.GroupID, impGroup.ID)
	}
	if impChild.ProxyID == nil {
		t.Fatal("child.ProxyID is nil after import")
	}
	impProxy, err := dst.proxies.GetByID(ctx, *impChild.ProxyID)
	if err != nil {
		t.Fatalf("get proxy: %v", err)
	}
	if impProxy.Password != "secret-proxy-pass" || impProxy.UserID != destUser {
		t.Fatalf("proxy remapped incorrectly: %+v", impProxy)
	}
	// IDs may coincidentally match across independent fakes (both start at 1);
	// what matters is relationship integrity inside the destination store.

	// Tag assignment remapped.
	mtags, err := dst.monitorTags.ListByMonitor(ctx, impChild.ID)
	if err != nil || len(mtags) != 1 {
		t.Fatalf("monitor tags: %v len=%d", err, len(mtags))
	}
	impTag, err := dst.tags.GetByID(ctx, mtags[0].TagID)
	if err != nil || impTag.Name != "prod" {
		t.Fatalf("tag: %v %+v", err, impTag)
	}

	// Notification link remapped.
	nlinks, err := dst.monitorNotifs.ListByMonitor(ctx, impChild.ID)
	if err != nil || len(nlinks) != 1 {
		t.Fatalf("notif links: %v", err)
	}
	impNotif, err := dst.notifs.GetByID(ctx, nlinks[0].NotificationID)
	if err != nil || impNotif.Config["bot_token"] != "123:ABC" {
		t.Fatalf("notif secrets not restored: %v %+v", err, impNotif)
	}

	// Status page monitor remapped.
	sps, err := dst.statusPages.List(ctx)
	if err != nil || len(sps) != 1 {
		t.Fatalf("status pages: %v", err)
	}
	if sps[0].SLATarget == nil || *sps[0].SLATarget != slaTarget {
		t.Fatalf("restored status page SLA target = %v; want %v", sps[0].SLATarget, slaTarget)
	}
	spLinks, err := dst.spMonitors.ListByStatusPage(ctx, sps[0].ID)
	if err != nil || len(spLinks) != 1 || spLinks[0].MonitorID != impChild.ID {
		t.Fatalf("sp monitors remapping failed: %v %+v", err, spLinks)
	}

	// Maintenance monitor remapped.
	mws, err := dst.maint.List(ctx, destUser)
	if err != nil || len(mws) != 1 {
		t.Fatalf("maint: %v", err)
	}
	mmids, err := dst.maintMonitors.ListByMaintenance(ctx, mws[0].ID)
	if err != nil || len(mmids) != 1 || mmids[0] != impChild.ID {
		t.Fatalf("maint monitors remapping failed: %v %+v", err, mmids)
	}
}

// TestBackupService_Import_MonitorGroups_ArbitraryDepth exercises a 4-level
// group chain (A -> B -> C -> D, deepest last) fed to Import in scrambled
// order — deliberately not parents-first — to prove the import handles
// arbitrary nesting depth rather than just one level. A monitor filed under
// the deepest group (D) must come out pointing at D's *new* ID.
func TestBackupService_Import_MonitorGroups_ArbitraryDepth(t *testing.T) {
	ctx := context.Background()
	h := newBackupHarness()

	idA, idB, idC, idD := int64(10), int64(20), int64(30), int64(40)
	doc := &BackupDocument{
		Version: BackupDocumentVersion,
		// Scrambled: D (deepest) first, A (root) last.
		MonitorGroups: []BackupMonitorGroup{
			{ID: idD, Name: "D", ParentID: &idC, Condition: domain.GroupConditionWorstOfChildren},
			{ID: idB, Name: "B", ParentID: &idA, Condition: domain.GroupConditionWorstOfChildren},
			{ID: idC, Name: "C", ParentID: &idB, Condition: domain.GroupConditionWorstOfChildren},
			{ID: idA, Name: "A", ParentID: nil, Condition: domain.GroupConditionWorstOfChildren},
		},
		Monitors: []BackupMonitor{
			{ID: 1, Name: "Deep", Type: "http", Active: true, Interval: 60, Timeout: 30,
				Config: map[string]any{}, GroupID: &idD},
		},
	}

	summary, err := h.svc.Import(ctx, 1, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(summary.Skipped) != 0 {
		t.Fatalf("unexpected skips: %+v", summary.Skipped)
	}
	if summary.MonitorGroupsCreated != 4 {
		t.Fatalf("monitor groups created = %d, want 4", summary.MonitorGroupsCreated)
	}
	if summary.MonitorsCreated != 1 {
		t.Fatalf("monitors created = %d, want 1", summary.MonitorsCreated)
	}

	groups, err := h.groups.List(ctx, 1)
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	byName := make(map[string]*domain.MonitorGroup, len(groups))
	for _, g := range groups {
		byName[g.Name] = g
	}
	a, b, c, d := byName["A"], byName["B"], byName["C"], byName["D"]
	if a == nil || b == nil || c == nil || d == nil {
		t.Fatalf("missing imported groups: %+v", byName)
	}
	if a.ParentID != nil {
		t.Fatalf("A.ParentID = %v, want nil (root)", a.ParentID)
	}
	if b.ParentID == nil || *b.ParentID != a.ID {
		t.Fatalf("B.ParentID = %v, want %d (A)", b.ParentID, a.ID)
	}
	if c.ParentID == nil || *c.ParentID != b.ID {
		t.Fatalf("C.ParentID = %v, want %d (B)", c.ParentID, b.ID)
	}
	if d.ParentID == nil || *d.ParentID != c.ID {
		t.Fatalf("D.ParentID = %v, want %d (C)", d.ParentID, c.ID)
	}

	monitors, err := h.monitors.List(ctx, ports.MonitorFilter{UserID: 1})
	if err != nil || len(monitors) != 1 {
		t.Fatalf("list monitors: %v len=%d", err, len(monitors))
	}
	if monitors[0].GroupID == nil || *monitors[0].GroupID != d.ID {
		t.Fatalf("monitor.GroupID = %v, want %d (D)", monitors[0].GroupID, d.ID)
	}
}

// TestBackupService_Import_MonitorGroups_DanglingParentSkipped ensures a
// group whose ParentID never resolves within the document (and any monitor
// that references it) is skipped rather than silently dropping the parent
// link or crashing the import.
func TestBackupService_Import_MonitorGroups_DanglingParentSkipped(t *testing.T) {
	ctx := context.Background()
	h := newBackupHarness()

	missingParent := int64(999)
	doc := &BackupDocument{
		Version:       BackupDocumentVersion,
		MonitorGroups: []BackupMonitorGroup{{ID: 1, Name: "Orphan", ParentID: &missingParent}},
		Monitors: []BackupMonitor{
			{ID: 1, Name: "M", Type: "http", Active: true, Interval: 60, Timeout: 30,
				Config: map[string]any{}, GroupID: func() *int64 { id := int64(1); return &id }()},
		},
	}
	summary, err := h.svc.Import(ctx, 1, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.MonitorGroupsCreated != 0 {
		t.Fatalf("monitor groups created = %d, want 0", summary.MonitorGroupsCreated)
	}
	if summary.MonitorsCreated != 0 {
		t.Fatalf("monitors created = %d, want 0 (group never imported)", summary.MonitorsCreated)
	}
	if len(summary.Skipped) != 2 {
		t.Fatalf("skipped = %+v, want 2 entries (group + monitor)", summary.Skipped)
	}
}

func TestBackupService_Import_ReusesExistingTagByName(t *testing.T) {
	ctx := context.Background()
	h := newBackupHarness()

	existing := &domain.Tag{Name: "prod", Color: "#111111"}
	if err := h.tags.Create(ctx, existing); err != nil {
		t.Fatalf("seed tag: %v", err)
	}

	doc := &BackupDocument{
		Version: BackupDocumentVersion,
		Tags:    []BackupTag{{ID: 50, Name: "prod", Color: "#ff0000"}},
		Monitors: []BackupMonitor{
			{ID: 1, Name: "M", Type: "http", Active: true, Interval: 60, Timeout: 30, Config: map[string]any{}},
		},
		MonitorTags: []BackupMonitorTag{{MonitorID: 1, TagID: 50, Value: "v"}},
	}
	summary, err := h.svc.Import(ctx, 1, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.TagsCreated != 0 || summary.TagsReused != 1 {
		t.Fatalf("want reuse, got created=%d reused=%d", summary.TagsCreated, summary.TagsReused)
	}
	all, _ := h.tags.List(ctx)
	if len(all) != 1 {
		t.Fatalf("tag count = %d, want 1", len(all))
	}
}

func TestBackupService_Import_SlugCollision(t *testing.T) {
	ctx := context.Background()
	h := newBackupHarness()
	if err := h.statusPages.Create(ctx, &domain.StatusPage{Slug: "public", Title: "Existing", Theme: "light"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Need a monitor so status page is importable with links (page itself still imports).
	doc := &BackupDocument{
		Version: BackupDocumentVersion,
		Monitors: []BackupMonitor{
			{ID: 1, Name: "M", Type: "http", Active: true, Interval: 60, Timeout: 30, Config: map[string]any{}},
		},
		StatusPages: []BackupStatusPage{
			{ID: 9, Slug: "public", Title: "Imported", Theme: "dark", Published: true},
		},
		StatusPageMonitors: []BackupStatusPageMonitor{
			{StatusPageID: 9, MonitorID: 1, DisplayOrder: 1},
		},
	}
	summary, err := h.svc.Import(ctx, 1, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.StatusPagesCreated != 1 {
		t.Fatalf("status pages created=%d skipped=%+v", summary.StatusPagesCreated, summary.Skipped)
	}
	sp, err := h.statusPages.GetBySlug(ctx, "public-imported")
	if err != nil {
		t.Fatalf("expected slug public-imported: %v", err)
	}
	if sp.Title != "Imported" {
		t.Fatalf("title = %q", sp.Title)
	}
}

func TestBackupService_Import_RejectsUnsupportedVersion(t *testing.T) {
	h := newBackupHarness()
	_, err := h.svc.Import(context.Background(), 1, &BackupDocument{Version: 99})
	if err == nil {
		t.Fatal("expected error for unsupported version")
	}
}

func TestBackupService_Export_ScopesToUser(t *testing.T) {
	ctx := context.Background()
	h := newBackupHarness()

	_ = h.monitors.Create(ctx, &domain.Monitor{UserID: 1, Name: "Mine", Type: "http", Interval: 60, Timeout: 30, Config: map[string]any{}})
	_ = h.monitors.Create(ctx, &domain.Monitor{UserID: 2, Name: "Theirs", Type: "http", Interval: 60, Timeout: 30, Config: map[string]any{}})
	_ = h.proxies.Create(ctx, &domain.Proxy{UserID: 1, Protocol: "http", Host: "a", Port: 1})
	_ = h.proxies.Create(ctx, &domain.Proxy{UserID: 2, Protocol: "http", Host: "b", Port: 2})

	doc, err := h.svc.Export(ctx, 1)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(doc.Monitors) != 1 || doc.Monitors[0].Name != "Mine" {
		t.Fatalf("monitors = %+v", doc.Monitors)
	}
	if len(doc.Proxies) != 1 || doc.Proxies[0].Host != "a" {
		t.Fatalf("proxies = %+v", doc.Proxies)
	}
}
