// Package services contains the use-case implementations.
// Services depend ONLY on ports and domain — never on adapters.
package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// MonitorService handles monitor CRUD and lifecycle operations.
type MonitorService struct {
	repo              ports.MonitorRepository
	bus               ports.EventBus
	proxyRepo         ports.ProxyRepository                  // optional: nil disables proxy_id validation
	groupRepo         ports.MonitorGroupRepository           // optional: nil disables group_id validation
	notifRepo         ports.NotificationRepository           // optional: for is_default auto-link
	monitorNotifRepo  ports.MonitorNotificationRepository    // optional: for is_default auto-link
	conditionRepo     ports.MonitorConditionRepository       // optional: removes disabled auxiliary conditions
	assignWriter      ports.ProbeAssignmentWriter            // optional: atomic create-with-assignments
	assignReader      ports.MonitorProbeAssignmentRepository // optional: clone honors the source set
	probeRegistry     ports.ProbeRegistryRepository          // optional: validates explicit members
	probeCapabilities ports.ProbeAssignmentCapabilities      // optional: validates remote executability
	fleetGate         FleetActivationGate                    // required for remote members: T34 mixed-version guard
}

// SetConditionRepository wires cleanup for auxiliary observations whose
// database capacity check has been disabled on an updated monitor.
func (s *MonitorService) SetConditionRepository(repo ports.MonitorConditionRepository) {
	s.conditionRepo = repo
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
	return s.create(ctx, m, true)
}

// CreateWithoutDefaultNotifications creates a monitor exactly as Create but
// without the new-monitor default notification links. Backup restore uses it:
// the imported document carries the complete notification graph (possibly
// deliberately empty) and the destination's default channels must not leak
// into it. Validation, provisioning and the monitor.update event are unchanged.
func (s *MonitorService) CreateWithoutDefaultNotifications(ctx context.Context, m *domain.Monitor) error {
	return s.create(ctx, m, false)
}

func (s *MonitorService) create(ctx context.Context, m *domain.Monitor, linkDefaults bool) error {
	normalizeHTTPMonitorURL(m)
	if err := ensurePushToken(m); err != nil {
		return fmt.Errorf("monitor service: create: %w", err)
	}
	// Default display order matches the schema DEFAULT (2000). Zero is treated
	// as "unset" on create so clients that omit the field still get a stable
	// middle-of-list weight rather than sorting ahead of every explicit value.
	if m.Weight == 0 {
		m.Weight = 2000
	}
	if err := s.validateGroup(ctx, m); err != nil {
		return err
	}
	if err := s.validateProxy(ctx, m); err != nil {
		return err
	}
	if err := s.repo.Create(ctx, m); err != nil {
		return fmt.Errorf("monitor service: create: %w", err)
	}
	if linkDefaults {
		if err := s.attachDefaultNotifications(ctx, m); err != nil {
			return fmt.Errorf("monitor service: create: attach defaults: %w", err)
		}
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
		if err := s.monitorNotifRepo.Attach(ctx, m.ID, n.ID, domain.DefaultIncludeTarget); err != nil {
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

// validateGroup ensures m.GroupID, when set, references an existing monitor
// group. Placement authorization is NOT checked here: AccessService decides
// whether the caller may create/move a monitor into a group (group grants),
// and that check lives in the HTTP handlers. Groups are shared folders under
// RBAC — a non-admin routinely files monitors into an admin-owned group they
// have been granted — so an ownership match would reject every legitimate
// scoped create.
func (s *MonitorService) validateGroup(ctx context.Context, m *domain.Monitor) error {
	if m.GroupID == nil {
		return nil
	}
	if s.groupRepo == nil {
		return fmt.Errorf("monitor service: %w: monitor group support is not enabled", domain.ErrValidation)
	}
	if _, err := s.groupRepo.GetByID(ctx, *m.GroupID); err != nil {
		if errors.Is(err, ports.ErrNotFound) || errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("monitor service: %w: group not found", domain.ErrValidation)
		}
		return fmt.Errorf("monitor service: validate group: %w", err)
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
	normalizePushToken(m)
	if err := s.validateGroup(ctx, m); err != nil {
		return err
	}
	if err := s.validateProxy(ctx, m); err != nil {
		return err
	}
	if err := s.repo.Update(ctx, m); err != nil {
		return fmt.Errorf("monitor service: update: %w", err)
	}
	// Disabled capacity checks are cleaned up after persist. Failure must not
	// tell the caller the monitor save failed — the row is already written.
	if err := s.syncMonitorConditions(ctx, m); err != nil {
		slog.ErrorContext(ctx, "monitor service: condition cleanup after update failed",
			"monitor_id", m.ID, "error", err)
	}
	_ = s.bus.Publish(ctx, ports.Event{Type: "monitor.update", Payload: m})
	return nil
}

func (s *MonitorService) syncMonitorConditions(ctx context.Context, m *domain.Monitor) error {
	if s.conditionRepo == nil || m == nil {
		return nil
	}
	checks := []struct {
		kind    string
		enabled bool
	}{
		{kind: domain.MonitorConditionSessionPool, enabled: m.Type == "database" && monitorConfigEnabled(m.Config, "check_session_pool")},
		{kind: domain.MonitorConditionStorage, enabled: m.Type == "database" && monitorConfigEnabled(m.Config, "check_storage")},
	}
	for _, check := range checks {
		if check.enabled {
			continue
		}
		if err := s.conditionRepo.DeleteKind(ctx, m.ID, check.kind); err != nil {
			return fmt.Errorf("delete %s: %w", check.kind, err)
		}
		_ = s.bus.Publish(ctx, ports.Event{
			Type:    "condition.delete",
			Payload: domain.ConditionDelete{MonitorID: m.ID, Kind: check.kind},
		})
	}
	return nil
}

func monitorConfigEnabled(config map[string]any, key string) bool {
	value, ok := config[key]
	if !ok {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
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

// normalizePushToken keeps the push ingestion lookup column
// (domain.Monitor.PushToken) and Config["push_token"] consistent. It runs at
// the shared service boundary used by ordinary and declarative workflows so
// both persist the same lookup value (issue #66).
//
// Contract:
//   - only push monitors carry a lookup token; other types are left untouched
//   - a real config value is authoritative and is copied into PushToken
//   - an omitted, empty, or ConfigSecretRedacted config value preserves the
//     persisted token and back-fills the config from it, so a redacted or
//     partial update can never erase or desynchronize a live lookup token
//   - with nothing to preserve, a redacted placeholder is dropped instead of
//     being persisted as if it were a real token
//
// Create paths wrap this with ensurePushToken so an empty result becomes a
// freshly generated token; update paths call this directly and never mint one.
func normalizePushToken(m *domain.Monitor) {
	if m == nil || m.Type != "push" {
		return
	}
	if m.Config == nil {
		m.Config = map[string]any{}
	}
	token, _ := m.Config["push_token"].(string)
	switch {
	case token != "" && token != ConfigSecretRedacted:
		m.PushToken = token
	case m.PushToken != "":
		m.Config["push_token"] = m.PushToken
	case token == ConfigSecretRedacted:
		delete(m.Config, "push_token")
	}
}

// ensurePushToken is the create-time wrapper around normalizePushToken: after
// syncing, a push monitor that still has no lookup token gets a freshly
// generated one. Every supported create path runs through it — ordinary create
// (which backup restore shares), create-with-assignments, and declarative apply
// — so a created push monitor always comes up with a working ingest token,
// whether the caller supplied one, omitted it, or sent the redacted sentinel
// (the API contract the UI reports as "generated"). Update paths keep the
// preserve-on-redaction semantics of normalizePushToken and never mint a token.
func ensurePushToken(m *domain.Monitor) error {
	normalizePushToken(m)
	if m == nil || m.Type != "push" || m.PushToken != "" {
		return nil
	}
	token, err := generatePushToken()
	if err != nil {
		return fmt.Errorf("generate push token: %w", err)
	}
	m.PushToken = token
	m.Config["push_token"] = token
	return nil
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
// generatePushToken returns a unique push ingest token for push monitors.
func generatePushToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
