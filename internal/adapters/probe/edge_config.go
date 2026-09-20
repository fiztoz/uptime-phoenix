package probe

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// EdgeConfigDecoder validates the M2 HTTP/TCP/DNS engineering runtime. Features
// assigned to M3/M4 fail explicitly until they have an execution owner.
type EdgeConfigDecoder struct{ validators *LocalConfigValidator }

var _ ports.EdgeConfigDecoder = (*EdgeConfigDecoder)(nil)

// NewEdgeConfigDecoder accepts the actual installed checker and sender lookups.
func NewEdgeConfigDecoder(checker func(string) (ports.Checker, bool), sender func(string) (ports.NotificationSender, bool)) *EdgeConfigDecoder {
	return &EdgeConfigDecoder{validators: NewLocalConfigValidator(checker, sender)}
}

// DecodeEdge validates both graph references and extension semantics, then maps
// the explicit wire whitelist into runtime data. No checker or provider runs here.
func (d *EdgeConfigDecoder) DecodeEdge(ctx context.Context, document []byte, target domain.ProbeConfigTarget) (*domain.EdgeResolvedConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d == nil || d.validators == nil || d.validators.checker == nil || d.validators.sender == nil || !domain.ValidProbeConfigTarget(target) || target.ProbeID == domain.LocalProbeID {
		return nil, domain.ErrValidation
	}
	s, err := DecodeConfigSnapshot(document)
	if err != nil || s.HubID != target.HubID || s.ProbeID != target.ProbeID {
		return nil, domain.ErrValidation
	}
	if err := validateEdgeRuntimeSnapshot(s); err != nil {
		return nil, err
	}
	if err := d.validators.validateSnapshot(ctx, s); err != nil {
		return nil, err
	}
	metadata, err := (ConfigInspector{}).Inspect(document, target)
	if err != nil {
		return nil, err
	}
	out := &domain.EdgeResolvedConfig{Metadata: metadata, Assignments: make([]domain.EdgeResolvedAssignment, 0, len(s.Assignments)), Channels: make(map[int64]domain.EdgeResolvedChannel), Templates: make(map[int64]*domain.NotificationTemplate), Maintenance: make(map[int64]*domain.MaintenanceWindow), Policies: make(map[int64]*domain.EscalationPolicy)}
	out.Watchdog = resolvedConfigWatchdog(s.Watchdog)
	if s.Probe != nil {
		out.Probe = domain.ProbeDisplay{Name: s.Probe.Name, Location: s.Probe.Location}
	}
	proxies := make(map[string]*domain.Proxy)
	for _, p := range s.ProxyBindings {
		proxies[p.BindingKey] = &domain.Proxy{Protocol: p.Protocol, Host: p.Host, Port: int(p.Port), Auth: p.Auth, Username: p.Username, Password: p.Password, Active: p.Active}
	}
	for _, a := range s.Assignments {
		config, err := configExtensionObject(a.Monitor.Config)
		if err != nil {
			return nil, domain.ErrValidation
		}
		m := a.Monitor
		resolved := domain.EdgeResolvedAssignment{Monitor: &domain.Monitor{ID: a.MonitorID, Name: m.Name, Description: m.Description, Owner: m.Owner, Type: m.Type, Active: a.Active, Interval: int(m.Interval), RetryInterval: int(m.RetryInterval), MaxRetries: int(m.MaxRetries), Timeout: m.Timeout, Config: config, AcceptedStatusCodes: slices.Clone(m.AcceptedStatusCodes), UpsideDown: m.UpsideDown, ResendInterval: int(m.ResendInterval), TLSIgnore: m.TLSIgnore}, Generation: int64(a.Generation), EffectiveOwner: m.EffectiveOwner, MaintenanceIDs: slices.Clone(a.MaintenanceIDs), EscalationPolicyID: a.EscalationPolicyID}
		for _, tag := range m.Tags {
			resolved.Tags = append(resolved.Tags, domain.ProbeConfigTag{Name: tag.Name, Value: tag.Value})
		}
		for _, link := range a.NotificationLinks {
			resolved.NotificationLinks = append(resolved.NotificationLinks, domain.MonitorNotification{MonitorID: a.MonitorID, NotificationID: link.NotificationID, IncludeTarget: link.IncludeTarget})
		}
		if a.ProxyBindingKey != nil {
			resolved.Proxy = proxies[*a.ProxyBindingKey]
		}
		out.Assignments = append(out.Assignments, resolved)
	}
	for _, c := range s.NotificationChannels {
		config, err := configExtensionObject(c.Config)
		if err != nil {
			return nil, domain.ErrValidation
		}
		out.Channels[c.ID] = domain.EdgeResolvedChannel{Version: int64(c.Version), Notification: &domain.Notification{ID: c.ID, Name: c.Name, Type: c.Type, Active: c.Active, Config: config, TemplateID: c.TemplateID, IncludeAckURL: c.IncludeAckURL}}
	}
	for _, t := range s.NotificationTemplates {
		config, err := configExtensionObject(t.Config)
		if err != nil {
			return nil, domain.ErrValidation
		}
		out.Templates[t.ID] = &domain.NotificationTemplate{ID: t.ID, Name: t.Name, Provider: t.Provider, TitleTemplate: t.TitleTemplate, BodyTemplate: t.BodyTemplate, Config: config}
	}
	for _, w := range s.MaintenanceWindows {
		window := &domain.MaintenanceWindow{ID: w.ID, Active: w.Active, Strategy: w.Strategy, CronExpr: w.CronExpr, Duration: int(w.Duration), Timezone: w.Timezone}
		if w.StartDate != nil {
			window.StartDate = time.Time(*w.StartDate).UTC()
		}
		if w.EndDate != nil {
			window.EndDate = time.Time(*w.EndDate).UTC()
		}
		out.Maintenance[w.ID] = window
	}
	for _, p := range s.EscalationPolicies {
		policy := &domain.EscalationPolicy{ID: p.ID, Enabled: p.Enabled}
		for _, step := range p.Steps {
			policy.Steps = append(policy.Steps, domain.EscalationStep{PolicyID: p.ID, StepOrder: int(step.Step), WaitMinutes: int(step.DelaySeconds) / 60, NotificationIDs: slices.Clone(step.NotificationIDs)})
		}
		out.Policies[p.ID] = policy
	}
	return out, ctx.Err()
}

func validateEdgeRuntimeSnapshot(s ConfigSnapshot) error {
	if s.Watchdog.Enabled {
		return fmt.Errorf("connection watchdog is unavailable in this build: %w", ErrUnsupportedCapability)
	}
	for _, a := range s.Assignments {
		if a.Monitor.Type != "http" && a.Monitor.Type != "tcp" && a.Monitor.Type != "dns" || len(a.ResourceBindings) != 0 {
			return fmt.Errorf("assignment requires an unavailable edge checker: %w", ErrUnsupportedCapability)
		}
		if a.Monitor.CertExpiryNotify {
			return fmt.Errorf("certificate paging is unavailable in this build: %w", ErrUnsupportedCapability)
		}
	}
	for _, c := range s.NotificationChannels {
		if c.IncludeAckURL {
			return fmt.Errorf("remote acknowledgement links are unavailable in this build: %w", ErrUnsupportedCapability)
		}
	}
	for _, p := range s.EscalationPolicies {
		if p.Enabled && len(p.Steps) != 0 {
			return fmt.Errorf("remote escalation is unavailable in this build: %w", ErrUnsupportedCapability)
		}
	}
	return nil
}
