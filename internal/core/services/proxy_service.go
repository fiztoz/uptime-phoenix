package services

import (
	"context"
	"fmt"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// validProxyProtocols lists the protocols ProxyService accepts.
//
// socks4 is intentionally excluded: golang.org/x/net/proxy (the only SOCKS
// dialer already vendored in this module — see internal/adapters/checker/http.go)
// only implements a SOCKS5 dialer. Hand-rolling a raw SOCKS4 client for a
// rarely-used protocol would violate the Minimal-Dependency Principle in
// AGENTS.md, so socks4 is rejected here with a clear validation error
// instead of being silently accepted and failing at check time.
var validProxyProtocols = map[string]bool{
	"http":   true,
	"https":  true,
	"socks5": true,
}

// ProxyService handles proxy CRUD scoped to the owning user and enforces the
// "at most one default proxy per user" invariant.
type ProxyService struct {
	repo ports.ProxyRepository
}

// NewProxyService creates a new ProxyService.
func NewProxyService(repo ports.ProxyRepository) *ProxyService {
	return &ProxyService{repo: repo}
}

// validate checks protocol/host/port invariants shared by Create and Update.
func (s *ProxyService) validate(p *domain.Proxy) error {
	if !validProxyProtocols[p.Protocol] {
		return fmt.Errorf("proxy service: %w: protocol must be one of http, https, socks5 (socks4 is not supported)", domain.ErrValidation)
	}
	if p.Host == "" {
		return fmt.Errorf("proxy service: %w: host is required", domain.ErrValidation)
	}
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("proxy service: %w: port must be between 1 and 65535", domain.ErrValidation)
	}
	return nil
}

// Create validates and creates a new proxy. If IsDefault is set, any other
// default proxy owned by the same user is cleared first so at most one
// default proxy exists per user.
func (s *ProxyService) Create(ctx context.Context, p *domain.Proxy) error {
	if err := s.validate(p); err != nil {
		return err
	}
	if p.IsDefault {
		if err := s.clearOtherDefaults(ctx, p.UserID, 0); err != nil {
			return err
		}
	}
	if err := s.repo.Create(ctx, p); err != nil {
		return fmt.Errorf("proxy service: create: %w", err)
	}
	return nil
}

// GetByID retrieves a proxy by its ID. Callers are responsible for verifying
// ownership (compare UserID) before exposing the result — mirrors how
// MaintenanceHandlers/TagHandlers enforce ownership in the handler layer.
func (s *ProxyService) GetByID(ctx context.Context, id int64) (*domain.Proxy, error) {
	return s.repo.GetByID(ctx, id)
}

// List retrieves every proxy owned by userID.
func (s *ProxyService) List(ctx context.Context, userID int64) ([]*domain.Proxy, error) {
	return s.repo.List(ctx, userID)
}

// Update validates and updates an existing proxy, enforcing the same
// single-default invariant as Create.
func (s *ProxyService) Update(ctx context.Context, p *domain.Proxy) error {
	if err := s.validate(p); err != nil {
		return err
	}
	if p.IsDefault {
		if err := s.clearOtherDefaults(ctx, p.UserID, p.ID); err != nil {
			return err
		}
	}
	if err := s.repo.Update(ctx, p); err != nil {
		return fmt.Errorf("proxy service: update: %w", err)
	}
	return nil
}

// Delete removes a proxy by ID. Monitors referencing it are not orphaned:
// the proxies.id -> monitors.proxy_id foreign key is declared
// ON DELETE SET NULL (see migrations/001_init.up.sql on both engines), so
// the database itself clears proxy_id on any monitor that used this proxy.
func (s *ProxyService) Delete(ctx context.Context, id int64) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		return fmt.Errorf("proxy service: delete: %w", err)
	}
	return nil
}

// clearOtherDefaults unsets IsDefault on every other proxy owned by userID so
// at most one default proxy exists per user. exceptID excludes the proxy
// currently being created/updated (0 when creating, since it has no ID yet).
func (s *ProxyService) clearOtherDefaults(ctx context.Context, userID, exceptID int64) error {
	existing, err := s.repo.List(ctx, userID)
	if err != nil {
		return fmt.Errorf("proxy service: list for default enforcement: %w", err)
	}
	for _, other := range existing {
		if other.ID == exceptID || !other.IsDefault {
			continue
		}
		other.IsDefault = false
		if err := s.repo.Update(ctx, other); err != nil {
			return fmt.Errorf("proxy service: clear default: %w", err)
		}
	}
	return nil
}
