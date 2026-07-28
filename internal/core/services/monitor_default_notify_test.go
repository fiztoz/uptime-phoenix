package services

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// fakeNotifRepo is a minimal NotificationRepository for default-link tests.
type fakeNotifRepo struct {
	mu   sync.Mutex
	byID map[int64]*domain.Notification
	next int64
}

func newFakeNotifRepo() *fakeNotifRepo {
	return &fakeNotifRepo{byID: make(map[int64]*domain.Notification)}
}

func (r *fakeNotifRepo) Create(_ context.Context, n *domain.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	n.ID = r.next
	if n.CreatedAt.IsZero() {
		n.CreatedAt = time.Now().UTC()
	}
	cp := *n
	r.byID[n.ID] = &cp
	return nil
}

func (r *fakeNotifRepo) GetByID(_ context.Context, id int64) (*domain.Notification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *n
	return &cp, nil
}

func (r *fakeNotifRepo) GetByMonitorID(_ context.Context, _ int64) ([]*domain.Notification, error) {
	return nil, nil
}

func (r *fakeNotifRepo) List(_ context.Context, userID int64) ([]*domain.Notification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Notification, 0, len(r.byID))
	for _, n := range r.byID {
		if n.UserID == userID {
			cp := *n
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *fakeNotifRepo) ListAll(_ context.Context) ([]*domain.Notification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Notification, 0, len(r.byID))
	for _, n := range r.byID {
		cp := *n
		out = append(out, &cp)
	}
	return out, nil
}

func (r *fakeNotifRepo) Update(_ context.Context, n *domain.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *n
	r.byID[n.ID] = &cp
	return nil
}

func (r *fakeNotifRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}

type fakeMonitorNotifLinkRepo struct {
	mu    sync.Mutex
	links []domain.MonitorNotification
}

func newFakeMonitorNotifLinkRepo() *fakeMonitorNotifLinkRepo {
	return &fakeMonitorNotifLinkRepo{}
}

func (r *fakeMonitorNotifLinkRepo) Attach(_ context.Context, monitorID, notificationID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.links = append(r.links, domain.MonitorNotification{
		MonitorID: monitorID, NotificationID: notificationID,
	})
	return nil
}

func (r *fakeMonitorNotifLinkRepo) Detach(_ context.Context, monitorID, notificationID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.links[:0]
	for _, l := range r.links {
		if !(l.MonitorID == monitorID && l.NotificationID == notificationID) {
			out = append(out, l)
		}
	}
	r.links = out
	return nil
}

func (r *fakeMonitorNotifLinkRepo) ListByMonitor(_ context.Context, monitorID int64) ([]*domain.MonitorNotification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.MonitorNotification
	for _, l := range r.links {
		if l.MonitorID == monitorID {
			cp := l
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *fakeMonitorNotifLinkRepo) ListByNotification(_ context.Context, notificationID int64) ([]*domain.MonitorNotification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.MonitorNotification
	for _, l := range r.links {
		if l.NotificationID == notificationID {
			cp := l
			out = append(out, &cp)
		}
	}
	return out, nil
}

// Creating a monitor must auto-link every is_default=true notification for
// the same owner, and must NOT link non-default ones.
func TestMonitorService_Create_AutoLinksDefaultNotifications(t *testing.T) {
	monRepo := newCloneFakeMonitorRepo()
	notifRepo := newFakeNotifRepo()
	linkRepo := newFakeMonitorNotifLinkRepo()
	bus := newFakeBus()
	svc := NewMonitorService(monRepo, bus)
	svc.SetDefaultNotificationLinker(notifRepo, linkRepo)
	ctx := context.Background()

	def := &domain.Notification{
		UserID: 1, Name: "default-discord", Type: "discord",
		Active: true, IsDefault: true, Config: map[string]any{"url": "https://x"},
	}
	nonDef := &domain.Notification{
		UserID: 1, Name: "manual-slack", Type: "slack",
		Active: true, IsDefault: false, Config: map[string]any{"url": "https://y"},
	}
	otherUser := &domain.Notification{
		UserID: 2, Name: "other-default", Type: "telegram",
		Active: true, IsDefault: true, Config: map[string]any{"token": "t"},
	}
	inactiveDef := &domain.Notification{
		UserID: 1, Name: "inactive-default", Type: "webhook",
		Active: false, IsDefault: true, Config: map[string]any{"url": "https://z"},
	}
	for _, n := range []*domain.Notification{def, nonDef, otherUser, inactiveDef} {
		if err := notifRepo.Create(ctx, n); err != nil {
			t.Fatalf("seed notification %s: %v", n.Name, err)
		}
	}

	m := &domain.Monitor{UserID: 1, Name: "API", Type: "http", Interval: 60, Active: true}
	if err := svc.Create(ctx, m); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if m.ID == 0 {
		t.Fatal("monitor ID not set after Create")
	}

	links, err := linkRepo.ListByMonitor(ctx, m.ID)
	if err != nil {
		t.Fatalf("ListByMonitor: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("auto-linked %d notifications; want exactly 1 (only active is_default for same user)", len(links))
	}
	if links[0].NotificationID != def.ID {
		t.Errorf("linked notification_id = %d; want default %d", links[0].NotificationID, def.ID)
	}
}

func TestMonitorService_Create_NoLinkerSkips(t *testing.T) {
	// Without SetDefaultNotificationLinker, Create still succeeds (tests/bootstrap partial).
	monRepo := newCloneFakeMonitorRepo()
	svc := NewMonitorService(monRepo, newFakeBus())
	m := &domain.Monitor{UserID: 1, Name: "X", Type: "http", Interval: 60}
	if err := svc.Create(context.Background(), m); err != nil {
		t.Fatalf("Create without linker: %v", err)
	}
}
