package services

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// --- fake alert repo -------------------------------------------------------

type fakeAlertRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.Alert
	nextID int64
}

func newFakeAlertRepo() *fakeAlertRepo {
	return &fakeAlertRepo{byID: make(map[int64]*domain.Alert), nextID: 1}
}

func (r *fakeAlertRepo) clone(a *domain.Alert) *domain.Alert {
	if a == nil {
		return nil
	}
	cp := *a
	if a.AckedAt != nil {
		t := *a.AckedAt
		cp.AckedAt = &t
	}
	if a.AckedByUserID != nil {
		id := *a.AckedByUserID
		cp.AckedByUserID = &id
	}
	if a.ResolvedAt != nil {
		t := *a.ResolvedAt
		cp.ResolvedAt = &t
	}
	if a.OpenMonitorID != nil {
		id := *a.OpenMonitorID
		cp.OpenMonitorID = &id
	}
	return &cp
}

func (r *fakeAlertRepo) Create(_ context.Context, a *domain.Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a.OpenMonitorID != nil {
		for _, existing := range r.byID {
			if existing.OpenMonitorID != nil && *existing.OpenMonitorID == *a.OpenMonitorID {
				return ports.ErrConflict
			}
		}
	}
	for _, existing := range r.byID {
		if existing.AckToken == a.AckToken {
			return ports.ErrConflict
		}
	}
	a.ID = r.nextID
	r.nextID++
	now := time.Now().UTC()
	a.CreatedAt = now
	a.UpdatedAt = now
	r.byID[a.ID] = r.clone(a)
	return nil
}

func (r *fakeAlertRepo) Update(_ context.Context, a *domain.Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[a.ID]; !ok {
		return ports.ErrNotFound
	}
	a.UpdatedAt = time.Now().UTC()
	r.byID[a.ID] = r.clone(a)
	return nil
}

func (r *fakeAlertRepo) GetByID(_ context.Context, id int64) (*domain.Alert, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return r.clone(a), nil
}

func (r *fakeAlertRepo) GetByAckToken(_ context.Context, token string) (*domain.Alert, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.byID {
		if a.AckToken == token {
			return r.clone(a), nil
		}
	}
	return nil, ports.ErrNotFound
}

func (r *fakeAlertRepo) GetOpenByMonitorID(_ context.Context, monitorID int64) (*domain.Alert, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.byID {
		if a.OpenMonitorID != nil && *a.OpenMonitorID == monitorID {
			return r.clone(a), nil
		}
	}
	return nil, ports.ErrNotFound
}

func (r *fakeAlertRepo) List(_ context.Context, filter ports.AlertFilter) ([]*domain.Alert, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Alert, 0, len(r.byID))
	for _, a := range r.byID {
		if filter.RestrictToMonitorIDs {
			allowed := false
			for _, id := range filter.MonitorIDs {
				if a.MonitorID == id {
					allowed = true
					break
				}
			}
			if !allowed {
				continue
			}
		}
		if filter.MonitorID != nil && a.MonitorID != *filter.MonitorID {
			continue
		}
		if filter.OpenOnly && !a.IsOpen() {
			continue
		}
		if len(filter.Statuses) > 0 {
			ok := false
			for _, s := range filter.Statuses {
				if a.Status == s {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		out = append(out, r.clone(a))
	}
	return out, nil
}

// --- tests -----------------------------------------------------------------

func TestAlertService_OpenAckResolve(t *testing.T) {
	repo := newFakeAlertRepo()
	svc := NewAlertService(repo)
	monitor := &domain.Monitor{ID: 7, Name: "api"}
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	a, err := svc.OpenOnDown(context.Background(), monitor, now)
	if err != nil {
		t.Fatalf("OpenOnDown: %v", err)
	}
	if a.Status != domain.AlertStatusFiring {
		t.Fatalf("status = %s; want firing", a.Status)
	}
	if a.AckToken == "" {
		t.Fatal("expected non-empty ack token")
	}
	if a.OpenMonitorID == nil || *a.OpenMonitorID != 7 {
		t.Fatalf("OpenMonitorID = %v; want 7", a.OpenMonitorID)
	}

	// Idempotent open while still open.
	again, err := svc.OpenOnDown(context.Background(), monitor, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("OpenOnDown again: %v", err)
	}
	if again.ID != a.ID {
		t.Fatalf("second open created id %d; want %d", again.ID, a.ID)
	}

	uid := int64(3)
	acked, err := svc.Acknowledge(context.Background(), a.ID, &uid)
	if err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if acked.Status != domain.AlertStatusAcked {
		t.Fatalf("status after ack = %s; want acked", acked.Status)
	}
	if acked.AckedByUserID == nil || *acked.AckedByUserID != 3 {
		t.Fatalf("AckedByUserID = %v; want 3", acked.AckedByUserID)
	}
	yes, err := svc.IsOpenAcked(context.Background(), 7)
	if err != nil || !yes {
		t.Fatalf("IsOpenAcked = %v, %v; want true, nil", yes, err)
	}

	// Idempotent re-ack.
	if _, err := svc.Acknowledge(context.Background(), a.ID, &uid); err != nil {
		t.Fatalf("re-ack: %v", err)
	}

	if err := svc.ResolveOpen(context.Background(), 7, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("ResolveOpen: %v", err)
	}
	yes, err = svc.IsOpenAcked(context.Background(), 7)
	if err != nil || yes {
		t.Fatalf("IsOpenAcked after resolve = %v, %v; want false, nil", yes, err)
	}
	resolved, err := svc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if resolved.Status != domain.AlertStatusResolved {
		t.Fatalf("status after resolve = %s; want resolved", resolved.Status)
	}
	if resolved.OpenMonitorID != nil {
		t.Fatalf("OpenMonitorID after resolve = %v; want nil", resolved.OpenMonitorID)
	}

	// New outage can open a fresh alert.
	next, err := svc.OpenOnDown(context.Background(), monitor, now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("OpenOnDown after resolve: %v", err)
	}
	if next.ID == a.ID {
		t.Fatal("expected a new alert id after resolve")
	}
}

func TestAlertService_AcknowledgeByToken(t *testing.T) {
	repo := newFakeAlertRepo()
	svc := NewAlertService(repo)
	a, err := svc.OpenOnDown(context.Background(), &domain.Monitor{ID: 1, Name: "x"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	acked, err := svc.AcknowledgeByToken(context.Background(), a.AckToken)
	if err != nil {
		t.Fatalf("AcknowledgeByToken: %v", err)
	}
	if acked.Status != domain.AlertStatusAcked {
		t.Fatalf("status = %s; want acked", acked.Status)
	}
	if acked.AckedByUserID != nil {
		t.Fatalf("token ack should not set user id; got %v", acked.AckedByUserID)
	}

	// After resolve, the token is spent.
	_ = svc.ResolveOpen(context.Background(), 1, time.Now().UTC())
	_, err = svc.AcknowledgeByToken(context.Background(), a.AckToken)
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("spent token err = %v; want ErrNotFound", err)
	}
}

func TestDispatcher_SuppressesResendWhileAcked(t *testing.T) {
	notif := &fakeNotifier{}
	repo := newFakeAlertRepo()
	alertSvc := NewAlertService(repo)
	d := newDispatcher(notif, &fakeMaintenance{})
	d.SetAlertLifecycle(alertSvc)

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := base
	d.now = func() time.Time { return clock }

	monitor := &domain.Monitor{ID: 1, Name: "api", ResendInterval: 5}

	// First DOWN opens alert + notifies.
	d.OnHeartbeat(context.Background(), monitor, &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}, ptrStatus(domain.StatusUp))
	if notif.count() != 1 {
		t.Fatalf("after first DOWN: count = %d; want 1", notif.count())
	}
	open, err := repo.GetOpenByMonitorID(context.Background(), 1)
	if err != nil {
		t.Fatalf("open alert: %v", err)
	}
	if _, err := alertSvc.Acknowledge(context.Background(), open.ID, nil); err != nil {
		t.Fatalf("ack: %v", err)
	}

	// Past resend window while acked — must NOT re-notify.
	clock = base.Add(10 * time.Minute)
	d.OnHeartbeat(context.Background(), monitor, &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}, ptrStatus(domain.StatusDown))
	if notif.count() != 1 {
		t.Fatalf("while acked: count = %d; want 1 (resend suppressed)", notif.count())
	}

	// Recovery resolves + notifies once.
	d.OnHeartbeat(context.Background(), monitor, &domain.Heartbeat{MonitorID: 1, Status: domain.StatusUp}, ptrStatus(domain.StatusDown))
	if notif.count() != 2 {
		t.Fatalf("after recovery: count = %d; want 2", notif.count())
	}
	// open is a clone from before resolve; re-read.
	got, err := repo.GetByID(context.Background(), open.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.AlertStatusResolved {
		t.Fatalf("status after recovery = %s; want resolved", got.Status)
	}
}

func TestDispatcher_ResendWithoutAckStillFires(t *testing.T) {
	notif := &fakeNotifier{}
	repo := newFakeAlertRepo()
	alertSvc := NewAlertService(repo)
	d := newDispatcher(notif, &fakeMaintenance{})
	d.SetAlertLifecycle(alertSvc)

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := base
	d.now = func() time.Time { return clock }

	monitor := &domain.Monitor{ID: 1, Name: "api", ResendInterval: 5}
	d.OnHeartbeat(context.Background(), monitor, &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}, ptrStatus(domain.StatusUp))
	clock = base.Add(6 * time.Minute)
	d.OnHeartbeat(context.Background(), monitor, &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}, ptrStatus(domain.StatusDown))
	if notif.count() != 2 {
		t.Fatalf("unacked resend: count = %d; want 2", notif.count())
	}
}
