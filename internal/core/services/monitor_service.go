// Package services contains the use-case implementations.
// Services depend ONLY on ports and domain — never on adapters.
package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// MonitorService handles monitor CRUD and lifecycle operations.
type MonitorService struct {
	repo             ports.MonitorRepository
	bus              ports.EventBus
	proxyRepo        ports.ProxyRepository               // optional: nil disables proxy_id validation
	groupRepo        ports.MonitorGroupRepository        // optional: nil disables group_id validation
	notifRepo        ports.NotificationRepository        // optional: for is_default auto-link
	monitorNotifRepo ports.MonitorNotificationRepository // optional: for is_default auto-link
}

// NewMonitorService creates a new MonitorService.
func NewMonitorService(repo ports.MonitorRepository, bus ports.EventBus) *MonitorService {
	return &MonitorService{repo: repo, bus: bus}
}

// SetProxyRepo attaches a proxy repository so monitor.ProxyID can be
// validated on create/update. Optional: when nil, a monitor with a non-nil
// ProxyID is rejected (see validateProxy) rather than silently accepted and
// left unresolved at check time. Mirrors HeartbeatService.SetTLSInfoRepo.
func (s *MonitorService) SetProxyRepo(repo ports.ProxyRepository) {
	s.proxyRepo = repo
}

// SetGroupRepo attaches a monitor group repository so monitor.GroupID can be
// validated on create/update. Optional: when nil, a monitor with a non-nil
// GroupID is rejected (see validateGroup) rather than silently accepted and
// left unresolved. Mirrors SetProxyRepo.
func (s *MonitorService) SetGroupRepo(repo ports.MonitorGroupRepository) {
	s.groupRepo = repo
}

// SetDefaultNotificationLinker wires notification repos so Create can auto-
// attach every is_default=true notification owned by the monitor's user.
// Optional: when unset, Create skips default linking (tests without notifs).
func (s *MonitorService) SetDefaultNotificationLinker(
	notifRepo ports.NotificationRepository,
	monitorNotifRepo ports.MonitorNotificationRepository,
) {
	s.notifRepo = notifRepo
	s.monitorNotifRepo = monitorNotifRepo
}

// Create creates a new monitor and publishes a monitor.update event.
// After persist, every active notification with is_default=true for the
// same owner is auto-linked (mirrors Proxy.IsDefault auto-selection).
func (s *MonitorService) Create(ctx context.Context, m *domain.Monitor) error {
	normalizeHTTPMonitorURL(m)
	if err := s.validateGroup(ctx, m); err != nil {
		return err
	}
	if err := s.validateProxy(ctx, m); err != nil {
		return err
	}
	if err := s.repo.Create(ctx, m); err != nil {
		return fmt.Errorf("monitor service: create: %w", err)
	}
	if err := s.attachDefaultNotifications(ctx, m); err != nil {
		return fmt.Errorf("monitor service: create: attach defaults: %w", err)
	}
	_ = s.bus.Publish(ctx, ports.Event{Type: "monitor.update", Payload: m})
	return nil
}

// attachDefaultNotifications links every active is_default notification for
// m.UserID onto the newly created monitor. No-op when linker repos are unset.
func (s *MonitorService) attachDefaultNotifications(ctx context.Context, m *domain.Monitor) error {
	if s.notifRepo == nil || s.monitorNotifRepo == nil {
		return nil
	}
	notifs, err := s.notifRepo.List(ctx, m.UserID)
	if err != nil {
		return err
	}
	for _, n := range notifs {
		if !n.IsDefault || !n.Active {
			continue
		}
		if err := s.monitorNotifRepo.Attach(ctx, m.ID, n.ID); err != nil {
			return fmt.Errorf("attach notification %d: %w", n.ID, err)
		}
	}
	return nil
}

// validateProxy ensures m.ProxyID, when set, references a proxy owned by the
// same user as m. Mirrored by validateGroup below.
func (s *MonitorService) validateProxy(ctx context.Context, m *domain.Monitor) error {
	if m.ProxyID == nil {
		return nil
	}
	if s.proxyRepo == nil {
		return fmt.Errorf("monitor service: %w: proxy support is not enabled", domain.ErrValidation)
	}
	proxy, err := s.proxyRepo.GetByID(ctx, *m.ProxyID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) || errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("monitor service: %w: proxy not found", domain.ErrValidation)
		}
		return fmt.Errorf("monitor service: validate proxy: %w", err)
	}
	if proxy.UserID != m.UserID {
		// Do not leak existence of another user's proxy — same message as "not found".
		return fmt.Errorf("monitor service: %w: proxy not found", domain.ErrValidation)
	}
	return nil
}

// validateGroup ensures m.GroupID, when set, references a monitor group
// owned by the same user as m. Mirrors validateProxy's ownership check.
func (s *MonitorService) validateGroup(ctx context.Context, m *domain.Monitor) error {
	if m.GroupID == nil {
		return nil
	}
	if s.groupRepo == nil {
		return fmt.Errorf("monitor service: %w: monitor group support is not enabled", domain.ErrValidation)
	}
	group, err := s.groupRepo.GetByID(ctx, *m.GroupID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) || errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("monitor service: %w: group not found", domain.ErrValidation)
		}
		return fmt.Errorf("monitor service: validate group: %w", err)
	}
	if group.UserID != m.UserID {
		// Do not leak existence of another user's group — same message as "not found".
		return fmt.Errorf("monitor service: %w: group not found", domain.ErrValidation)
	}
	return nil
}

// GetByID retrieves a monitor by its ID.
func (s *MonitorService) GetByID(ctx context.Context, id int64) (*domain.Monitor, error) {
	return s.repo.GetByID(ctx, id)
}

// GetByPushToken retrieves a monitor by its push token (used by the public push ingest endpoint).
func (s *MonitorService) GetByPushToken(ctx context.Context, pushToken string) (*domain.Monitor, error) {
	return s.repo.GetByPushToken(ctx, pushToken)
}

// List retrieves monitors matching the given filter.
func (s *MonitorService) List(ctx context.Context, filter ports.MonitorFilter) ([]*domain.Monitor, error) {
	return s.repo.List(ctx, filter)
}

// ListActive retrieves all active monitors.
func (s *MonitorService) ListActive(ctx context.Context) ([]*domain.Monitor, error) {
	return s.repo.ListActive(ctx)
}

// Update updates a monitor and publishes a monitor.update event.
func (s *MonitorService) Update(ctx context.Context, m *domain.Monitor) error {
	normalizeHTTPMonitorURL(m)
	if err := s.validateGroup(ctx, m); err != nil {
		return err
	}
	if err := s.validateProxy(ctx, m); err != nil {
		return err
	}
	if err := s.repo.Update(ctx, m); err != nil {
		return fmt.Errorf("monitor service: update: %w", err)
	}
	_ = s.bus.Publish(ctx, ports.Event{Type: "monitor.update", Payload: m})
	return nil
}

// normalizeHTTPMonitorURL makes the secure scheme explicit for HTTP monitors.
// This runs at the service boundary so monitors created through the REST API,
// imports, or future adapters all persist the same checker-ready URL.
func normalizeHTTPMonitorURL(m *domain.Monitor) {
	if m == nil || m.Type != "http" || m.Config == nil {
		return
	}
	raw, ok := m.Config["url"].(string)
	if !ok {
		return
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		m.Config["url"] = raw
		return
	}
	if strings.HasPrefix(raw, "//") {
		m.Config["url"] = "https:" + raw
		return
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	m.Config["url"] = raw
}

// Delete deletes a monitor by its ID and publishes a monitor.delete event.
func (s *MonitorService) Delete(ctx context.Context, id int64) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		return fmt.Errorf("monitor service: delete: %w", err)
	}
	_ = s.bus.Publish(ctx, ports.Event{Type: "monitor.delete", Payload: id})
	return nil
}

// Clone duplicates a monitor configuration for the given user.
func (s *MonitorService) Clone(ctx context.Context, id, userID int64) (*domain.Monitor, error) {
	src, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("monitor service: clone: %w", err)
	}
	if src.UserID != userID {
		return nil, domain.ErrNotFound
	}
	clone := *src
	clone.ID = 0
	clone.Name = src.Name + " (copy)"
	clone.UserID = userID
	clone.PushToken = ""
	// clone.GroupID is intentionally left as copied from src: cloning a
	// monitor filed under a folder should produce a copy that stays in that
	// same folder, not one that gets kicked out to top-level.
	if clone.Config != nil {
		cfg := make(map[string]any, len(clone.Config))
		for k, v := range clone.Config {
			cfg[k] = v
		}
		if clone.Type == "push" {
			token, err := generatePushToken()
			if err != nil {
				return nil, fmt.Errorf("monitor service: clone: generate push token: %w", err)
			}
			cfg["push_token"] = token
			clone.PushToken = token
		} else {
			delete(cfg, "push_token")
		}
		clone.Config = cfg
	}
	if err := s.Create(ctx, &clone); err != nil {
		return nil, fmt.Errorf("monitor service: clone: %w", err)
	}
	return &clone, nil
}

// generatePushToken returns a unique push ingest token for push monitors.
func generatePushToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
