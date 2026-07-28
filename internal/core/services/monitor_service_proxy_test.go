package services

import (
	"context"
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// TestMonitorService_Create_ProxyValidation covers validateProxy: the
// referenced proxy must exist and belong to the same user as the monitor.
// Mirrors TestMonitorService_Create_ParentValidation.
func TestMonitorService_Create_ProxyValidation(t *testing.T) {
	monRepo := newCloneFakeMonitorRepo()
	proxyRepo := newFakeProxyRepo()
	bus := newFakeBus()
	svc := NewMonitorService(monRepo, bus)
	svc.SetProxyRepo(proxyRepo)
	ctx := context.Background()

	ownProxy := &domain.Proxy{UserID: 1, Protocol: "http", Host: "proxy.local", Port: 8080, Active: true}
	if err := proxyRepo.Create(ctx, ownProxy); err != nil {
		t.Fatalf("seed own proxy: %v", err)
	}
	otherProxy := &domain.Proxy{UserID: 2, Protocol: "http", Host: "other.local", Port: 8080, Active: true}
	if err := proxyRepo.Create(ctx, otherProxy); err != nil {
		t.Fatalf("seed other user's proxy: %v", err)
	}

	t.Run("valid proxy succeeds", func(t *testing.T) {
		m := &domain.Monitor{UserID: 1, Name: "M1", Type: "http", Interval: 60, ProxyID: &ownProxy.ID}
		if err := svc.Create(ctx, m); err != nil {
			t.Fatalf("create with valid proxy: %v", err)
		}
		if m.ProxyID == nil || *m.ProxyID != ownProxy.ID {
			t.Fatalf("m.ProxyID = %v, want %d", m.ProxyID, ownProxy.ID)
		}
	})

	t.Run("nonexistent proxy rejected", func(t *testing.T) {
		missing := int64(999)
		m := &domain.Monitor{UserID: 1, Name: "M2", Type: "http", Interval: 60, ProxyID: &missing}
		err := svc.Create(ctx, m)
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Create(missing proxy) error = %v, want domain.ErrValidation", err)
		}
	})

	t.Run("cross-tenant proxy rejected", func(t *testing.T) {
		m := &domain.Monitor{UserID: 1, Name: "M3", Type: "http", Interval: 60, ProxyID: &otherProxy.ID}
		err := svc.Create(ctx, m)
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Create(cross-tenant proxy) error = %v, want domain.ErrValidation", err)
		}
	})

	t.Run("nil proxy always valid", func(t *testing.T) {
		m := &domain.Monitor{UserID: 1, Name: "M4", Type: "http", Interval: 60}
		if err := svc.Create(ctx, m); err != nil {
			t.Fatalf("create without proxy: %v", err)
		}
	})

	t.Run("proxy repo not wired rejects a non-nil proxy id", func(t *testing.T) {
		svcNoProxy := NewMonitorService(monRepo, bus) // SetProxyRepo never called
		m := &domain.Monitor{UserID: 1, Name: "M5", Type: "http", Interval: 60, ProxyID: &ownProxy.ID}
		err := svcNoProxy.Create(ctx, m)
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Create(proxy support disabled) error = %v, want domain.ErrValidation", err)
		}
	})
}

// TestMonitorService_Update_ProxyValidation covers the update path,
// including clearing an existing proxy_id by setting it to nil.
func TestMonitorService_Update_ProxyValidation(t *testing.T) {
	monRepo := newCloneFakeMonitorRepo()
	proxyRepo := newFakeProxyRepo()
	bus := newFakeBus()
	svc := NewMonitorService(monRepo, bus)
	svc.SetProxyRepo(proxyRepo)
	ctx := context.Background()

	ownProxy := &domain.Proxy{UserID: 1, Protocol: "http", Host: "proxy.local", Port: 8080, Active: true}
	if err := proxyRepo.Create(ctx, ownProxy); err != nil {
		t.Fatalf("seed proxy: %v", err)
	}

	m := &domain.Monitor{UserID: 1, Name: "M", Type: "http", Interval: 60}
	if err := svc.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Run("assigning a valid proxy succeeds", func(t *testing.T) {
		m.ProxyID = &ownProxy.ID
		if err := svc.Update(ctx, m); err != nil {
			t.Fatalf("update(assign proxy): %v", err)
		}
	})

	t.Run("clearing the proxy succeeds", func(t *testing.T) {
		m.ProxyID = nil
		if err := svc.Update(ctx, m); err != nil {
			t.Fatalf("update(clear proxy): %v", err)
		}
	})

	t.Run("assigning another user's proxy rejected", func(t *testing.T) {
		otherProxy := &domain.Proxy{UserID: 2, Protocol: "http", Host: "other.local", Port: 8080, Active: true}
		if err := proxyRepo.Create(ctx, otherProxy); err != nil {
			t.Fatalf("seed other user's proxy: %v", err)
		}
		m.ProxyID = &otherProxy.ID
		err := svc.Update(ctx, m)
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Update(cross-tenant proxy) error = %v, want domain.ErrValidation", err)
		}
		m.ProxyID = nil // reset for any subsequent subtests
	})
}
