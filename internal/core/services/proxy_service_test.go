package services

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// --- In-memory proxy repo for tests ---------------------------------------

type fakeProxyRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.Proxy
	nextID int64
}

func newFakeProxyRepo() *fakeProxyRepo {
	return &fakeProxyRepo{byID: make(map[int64]*domain.Proxy)}
}

func (r *fakeProxyRepo) Create(_ context.Context, p *domain.Proxy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	p.ID = r.nextID
	cp := *p
	r.byID[p.ID] = &cp
	return nil
}

func (r *fakeProxyRepo) GetByID(_ context.Context, id int64) (*domain.Proxy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (r *fakeProxyRepo) List(_ context.Context, userID int64) ([]*domain.Proxy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Proxy, 0)
	for _, p := range r.byID {
		if userID == 0 || p.UserID == userID {
			cp := *p
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *fakeProxyRepo) Update(_ context.Context, p *domain.Proxy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[p.ID]; !ok {
		return ports.ErrNotFound
	}
	cp := *p
	r.byID[p.ID] = &cp
	return nil
}

func (r *fakeProxyRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[id]; !ok {
		return ports.ErrNotFound
	}
	delete(r.byID, id)
	return nil
}

// --- Validation tests ------------------------------------------------------

func TestProxyService_Create_ValidatesProtocol(t *testing.T) {
	svc := NewProxyService(newFakeProxyRepo())

	cases := []struct {
		name     string
		protocol string
		wantErr  bool
	}{
		{"http accepted", "http", false},
		{"https accepted", "https", false},
		{"socks5 accepted", "socks5", false},
		{"socks4 rejected", "socks4", true},
		{"garbage rejected", "ftp", true},
		{"empty rejected", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &domain.Proxy{UserID: 1, Protocol: tc.protocol, Host: "proxy.local", Port: 8080}
			err := svc.Create(context.Background(), p)
			if tc.wantErr && !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Create(protocol=%q) error = %v, want domain.ErrValidation", tc.protocol, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Create(protocol=%q) unexpected error: %v", tc.protocol, err)
			}
		})
	}
}

func TestProxyService_Create_ValidatesHost(t *testing.T) {
	svc := NewProxyService(newFakeProxyRepo())
	p := &domain.Proxy{UserID: 1, Protocol: "http", Host: "", Port: 8080}
	err := svc.Create(context.Background(), p)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Create(empty host) error = %v, want domain.ErrValidation", err)
	}
}

func TestProxyService_Create_ValidatesPort(t *testing.T) {
	svc := NewProxyService(newFakeProxyRepo())

	cases := []struct {
		name    string
		port    int
		wantErr bool
	}{
		{"zero rejected", 0, true},
		{"negative rejected", -1, true},
		{"too large rejected", 65536, true},
		{"min accepted", 1, false},
		{"max accepted", 65535, false},
		{"typical accepted", 8080, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &domain.Proxy{UserID: 1, Protocol: "http", Host: "proxy.local", Port: tc.port}
			err := svc.Create(context.Background(), p)
			if tc.wantErr && !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Create(port=%d) error = %v, want domain.ErrValidation", tc.port, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Create(port=%d) unexpected error: %v", tc.port, err)
			}
		})
	}
}

// --- Single-default-per-user invariant --------------------------------------

func TestProxyService_Create_EnforcesSingleDefaultPerUser(t *testing.T) {
	repo := newFakeProxyRepo()
	svc := NewProxyService(repo)
	ctx := context.Background()

	first := &domain.Proxy{UserID: 1, Protocol: "http", Host: "a.local", Port: 8080, IsDefault: true}
	if err := svc.Create(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	second := &domain.Proxy{UserID: 1, Protocol: "http", Host: "b.local", Port: 8080, IsDefault: true}
	if err := svc.Create(ctx, second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	got, err := repo.GetByID(ctx, first.ID)
	if err != nil {
		t.Fatalf("get first: %v", err)
	}
	if got.IsDefault {
		t.Errorf("first proxy IsDefault = true; want false (should be cleared by second's creation)")
	}
	got2, err := repo.GetByID(ctx, second.ID)
	if err != nil {
		t.Fatalf("get second: %v", err)
	}
	if !got2.IsDefault {
		t.Errorf("second proxy IsDefault = false; want true")
	}
}

func TestProxyService_Create_DifferentUsersEachKeepOwnDefault(t *testing.T) {
	repo := newFakeProxyRepo()
	svc := NewProxyService(repo)
	ctx := context.Background()

	u1 := &domain.Proxy{UserID: 1, Protocol: "http", Host: "a.local", Port: 8080, IsDefault: true}
	if err := svc.Create(ctx, u1); err != nil {
		t.Fatalf("create u1: %v", err)
	}
	u2 := &domain.Proxy{UserID: 2, Protocol: "http", Host: "b.local", Port: 8080, IsDefault: true}
	if err := svc.Create(ctx, u2); err != nil {
		t.Fatalf("create u2: %v", err)
	}

	got1, _ := repo.GetByID(ctx, u1.ID)
	if !got1.IsDefault {
		t.Errorf("user1's proxy IsDefault = false; want true (unaffected by user2's default)")
	}
	got2, _ := repo.GetByID(ctx, u2.ID)
	if !got2.IsDefault {
		t.Errorf("user2's proxy IsDefault = false; want true")
	}
}

func TestProxyService_Update_EnforcesSingleDefaultPerUser(t *testing.T) {
	repo := newFakeProxyRepo()
	svc := NewProxyService(repo)
	ctx := context.Background()

	first := &domain.Proxy{UserID: 1, Protocol: "http", Host: "a.local", Port: 8080, IsDefault: true}
	if err := svc.Create(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	second := &domain.Proxy{UserID: 1, Protocol: "http", Host: "b.local", Port: 8080, IsDefault: false}
	if err := svc.Create(ctx, second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	// Flip second to default via Update — first must lose default status.
	second.IsDefault = true
	if err := svc.Update(ctx, second); err != nil {
		t.Fatalf("update second: %v", err)
	}

	got1, _ := repo.GetByID(ctx, first.ID)
	if got1.IsDefault {
		t.Errorf("first proxy IsDefault = true after second was promoted; want false")
	}
	got2, _ := repo.GetByID(ctx, second.ID)
	if !got2.IsDefault {
		t.Errorf("second proxy IsDefault = false; want true")
	}
}

func TestProxyService_Delete(t *testing.T) {
	repo := newFakeProxyRepo()
	svc := NewProxyService(repo)
	ctx := context.Background()

	p := &domain.Proxy{UserID: 1, Protocol: "http", Host: "a.local", Port: 8080}
	if err := svc.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Delete(ctx, p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.GetByID(ctx, p.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("GetByID after delete error = %v, want ports.ErrNotFound", err)
	}
}
