package probe

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// LocalConfigEncoder emits the explicit internal local snapshot dialect. It
// exposes no user, administrative, push-token or database-only model fields.
type LocalConfigEncoder struct{}

var _ ports.LocalProbeConfigEncoder = LocalConfigEncoder{}

// EncodeLocal serializes a deterministic, bounded complete dependency closure.
// The result contains credentials and must only enter the protected store.
func (LocalConfigEncoder) EncodeLocal(d domain.LocalProbeConfigDefinition) ([]byte, error) {
	if d.Target.ProbeID != domain.LocalProbeID || !domain.ValidProbeConfigTarget(d.Target) || d.Revision <= 0 || d.CreatedAt.IsZero() || d.EffectiveAt.IsZero() ||
		len(d.Assignments) > MaxConfigAssignments || len(d.Notifications) > MaxConfigDependencies || len(d.Templates) > MaxConfigDependencies || len(d.Policies) > MaxConfigDependencies || len(d.Proxies) > MaxConfigDependencies || len(d.Maintenance) > MaxConfigDependencies {
		return nil, domain.ErrValidation
	}
	s := ConfigSnapshot{
		SchemaVersion: 1, HubID: d.Target.HubID, ProbeID: d.Target.ProbeID, Revision: Decimal(d.Revision),
		CreatedAt: configTime(d.CreatedAt), EffectiveAt: configTime(d.EffectiveAt),
		Assignments: []ConfigAssignment{}, NotificationChannels: []ConfigChannel{}, NotificationTemplates: []ConfigTemplate{},
		MaintenanceWindows: []ConfigMaintenance{}, ProxyBindings: []ConfigProxy{}, EscalationPolicies: []ConfigEscalation{},
		// The local hub has no source connection watchdog. Remote settings need
		// their own authoritative configuration before a remote builder is enabled.
		Watchdog: ConfigWatchdog{LostAfterSeconds: 90, RecoverAfterSeconds: 30, NotificationIDs: []int64{}},
	}
	for _, assignment := range d.Assignments {
		a, err := encodeLocalAssignment(assignment)
		if err != nil {
			return nil, err
		}
		s.Assignments = append(s.Assignments, a)
	}
	for _, n := range d.Notifications {
		if n == nil {
			return nil, domain.ErrValidation
		}
		config, err := encodeConfigObject(n.Config, MaxConfigObjectBytes)
		if err != nil {
			return nil, err
		}
		s.NotificationChannels = append(s.NotificationChannels, ConfigChannel{ID: n.ID, Version: s.Revision, Type: n.Type, Name: n.Name, Active: n.Active, Config: config, TemplateID: n.TemplateID, IncludeAckURL: n.IncludeAckURL})
	}
	for _, template := range d.Templates {
		if template == nil {
			return nil, domain.ErrValidation
		}
		config, err := encodeConfigObject(template.Config, MaxTemplateConfigBytes)
		if err != nil {
			return nil, err
		}
		s.NotificationTemplates = append(s.NotificationTemplates, ConfigTemplate{ID: template.ID, Version: s.Revision, Provider: template.Provider, Name: template.Name, TitleTemplate: template.TitleTemplate, BodyTemplate: template.BodyTemplate, Config: config})
	}
	for _, proxy := range d.Proxies {
		if proxy == nil || !configInt32(proxy.Port) || proxy.ID <= 0 {
			return nil, domain.ErrValidation
		}
		s.ProxyBindings = append(s.ProxyBindings, ConfigProxy{BindingKey: localProxyKey(proxy.ID), Version: s.Revision, Protocol: proxy.Protocol, Host: proxy.Host, Port: int32(proxy.Port), Auth: proxy.Auth, Username: proxy.Username, Password: proxy.Password, Active: proxy.Active})
	}
	for _, entry := range d.Maintenance {
		w := entry.Window
		if w == nil || !configInt32(w.Duration) {
			return nil, domain.ErrValidation
		}
		window := ConfigMaintenance{ID: w.ID, Active: w.Active, Strategy: w.Strategy, CronExpr: w.CronExpr, Duration: int32(w.Duration), Timezone: w.Timezone, MonitorIDs: sortedConfigList(entry.MonitorIDs)}
		if w.Strategy == "single" {
			start, end := configTime(w.StartDate), configTime(w.EndDate)
			window.StartDate, window.EndDate = &start, &end
		}
		s.MaintenanceWindows = append(s.MaintenanceWindows, window)
	}
	for _, policy := range d.Policies {
		if policy == nil {
			return nil, domain.ErrValidation
		}
		p := ConfigEscalation{ID: policy.ID, Version: s.Revision, Enabled: policy.Enabled, Steps: []ConfigEscalationStep{}}
		for _, step := range policy.Steps {
			if !configInt32(step.StepOrder) || step.WaitMinutes < 0 || step.WaitMinutes > 7*24*60 {
				return nil, domain.ErrValidation
			}
			p.Steps = append(p.Steps, ConfigEscalationStep{Step: int32(step.StepOrder), DelaySeconds: int32(step.WaitMinutes * 60), NotificationIDs: sortedConfigList(step.NotificationIDs)})
		}
		slices.SortFunc(p.Steps, func(a, b ConfigEscalationStep) int { return compareConfigID(int64(a.Step), int64(b.Step)) })
		s.EscalationPolicies = append(s.EscalationPolicies, p)
	}
	sortLocalSnapshot(&s)
	document, err := json.Marshal(s)
	if err != nil || len(document) > MaxConfigSnapshotBytes {
		return nil, domain.ErrValidation
	}
	if _, err := DecodeLocalConfigSnapshot(document); err != nil {
		// Do not forward decoder errors that could include confidential fields.
		return nil, fmt.Errorf("invalid complete local configuration: %w", domain.ErrValidation)
	}
	return document, nil
}

func encodeLocalAssignment(a domain.ProbeConfigAssignment) (ConfigAssignment, error) {
	var out ConfigAssignment
	m := a.Monitor
	if m == nil || !configInt32(m.Interval) || !configInt32(m.RetryInterval) || !configInt32(m.MaxRetries) || !configInt32(m.ResendInterval) {
		return out, domain.ErrValidation
	}
	config, err := encodeConfigObject(m.Config, MaxConfigObjectBytes)
	if err != nil {
		return out, err
	}
	out = ConfigAssignment{MonitorID: m.ID, Generation: Decimal(a.Generation), Active: m.Active,
		Monitor: ConfigMonitor{Name: m.Name, Description: m.Description, Owner: m.Owner, EffectiveOwner: a.EffectiveOwner,
			Type: m.Type, Interval: int32(m.Interval), RetryInterval: int32(m.RetryInterval), MaxRetries: int32(m.MaxRetries), Timeout: m.Timeout,
			Config: config, AcceptedStatusCodes: append([]string{}, m.AcceptedStatusCodes...), UpsideDown: m.UpsideDown, TLSIgnore: m.TLSIgnore,
			CertExpiryNotify: m.CertExpiryNotify, ResendInterval: int32(m.ResendInterval), Tags: []ConfigTag{}},
		NotificationIDs: []int64{}, NotificationLinks: []ConfigNotificationLink{}, MaintenanceIDs: sortedConfigList(a.MaintenanceIDs),
		ResourceBindings: []ResourceBinding{}, EscalationPolicyID: a.EscalationPolicyID, RequiredCapabilities: []string{"checker." + m.Type + ".v1"},
	}
	for _, tag := range a.Tags {
		out.Monitor.Tags = append(out.Monitor.Tags, ConfigTag{Name: tag.Name, Value: tag.Value})
	}
	slices.SortFunc(out.Monitor.Tags, func(a, b ConfigTag) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	for _, link := range a.NotificationLinks {
		if link.MonitorID != m.ID {
			return out, domain.ErrValidation
		}
		out.NotificationIDs = append(out.NotificationIDs, link.NotificationID)
		out.NotificationLinks = append(out.NotificationLinks, ConfigNotificationLink{NotificationID: link.NotificationID, IncludeTarget: link.IncludeTarget})
	}
	slices.Sort(out.NotificationIDs)
	slices.SortFunc(out.NotificationLinks, func(a, b ConfigNotificationLink) int { return compareConfigID(a.NotificationID, b.NotificationID) })
	if m.ProxyID != nil {
		if *m.ProxyID <= 0 {
			return out, domain.ErrValidation
		}
		key := localProxyKey(*m.ProxyID)
		out.ProxyBindingKey = &key
	}
	return out, nil
}

func encodeConfigObject(value map[string]any, limit int) (json.RawMessage, error) {
	if value == nil {
		return json.RawMessage("{}"), nil
	}
	data, err := json.Marshal(value)
	if err != nil || len(data) > limit {
		return nil, domain.ErrValidation
	}
	return data, nil
}

func configInt32(value int) bool { return value >= 0 && int64(value) <= math.MaxInt32 }

func configTime(value time.Time) Timestamp { return Timestamp(value.UTC().Truncate(time.Microsecond)) }

func localProxyKey(id int64) string { return "proxy-" + strconv.FormatInt(id, 10) }

func sortedConfigList(ids []int64) []int64 {
	result := append([]int64{}, ids...)
	slices.Sort(result)
	return result
}

func compareConfigID(a, b int64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func sortLocalSnapshot(s *ConfigSnapshot) {
	slices.SortFunc(s.Assignments, func(a, b ConfigAssignment) int { return compareConfigID(a.MonitorID, b.MonitorID) })
	slices.SortFunc(s.NotificationChannels, func(a, b ConfigChannel) int { return compareConfigID(a.ID, b.ID) })
	slices.SortFunc(s.NotificationTemplates, func(a, b ConfigTemplate) int { return compareConfigID(a.ID, b.ID) })
	slices.SortFunc(s.MaintenanceWindows, func(a, b ConfigMaintenance) int { return compareConfigID(a.ID, b.ID) })
	slices.SortFunc(s.EscalationPolicies, func(a, b ConfigEscalation) int { return compareConfigID(a.ID, b.ID) })
	slices.SortFunc(s.ProxyBindings, func(a, b ConfigProxy) int {
		if a.BindingKey < b.BindingKey {
			return -1
		}
		if a.BindingKey > b.BindingKey {
			return 1
		}
		return 0
	})
}
