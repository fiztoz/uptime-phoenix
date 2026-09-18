package probe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const (
	// MaxConfigSnapshotBytes bounds reconstructed confidential configuration.
	MaxConfigSnapshotBytes = domain.MaxProbeConfigBytes
	// MaxConfigEntryBytes bounds each assignment/dependency before typed decoding.
	MaxConfigEntryBytes = 256 << 10
	// MaxConfigAssignments bounds a probe's complete assignment set.
	MaxConfigAssignments = 10000
	// MaxConfigDependencies bounds each dependency collection and reference list.
	MaxConfigDependencies = 1000
	// MaxConfigObjectBytes bounds checker/provider extension objects.
	MaxConfigObjectBytes = 64 << 10
	// MaxTemplateConfigBytes accommodates the existing structured HTML templates.
	MaxTemplateConfigBytes = 192 << 10
	// MaxConfigTags bounds template/display metadata per monitor.
	MaxConfigTags    = 256
	maxConfigMinutes = int64(math.MaxInt64 / time.Minute)
)

// ConfigSnapshot is a complete replacement document, never an API response.
// It contains secrets. Decode/assembly does not authorize execution or activation.
type ConfigSnapshot struct {
	SchemaVersion         int                 `json:"schema_version"`
	HubID                 string              `json:"hub_id"`
	ProbeID               string              `json:"probe_id"`
	Revision              Decimal             `json:"revision"`
	CreatedAt             Timestamp           `json:"created_at"`
	EffectiveAt           Timestamp           `json:"effective_at"`
	Assignments           []ConfigAssignment  `json:"assignments"`
	NotificationChannels  []ConfigChannel     `json:"notification_channels"`
	NotificationTemplates []ConfigTemplate    `json:"notification_templates"`
	MaintenanceWindows    []ConfigMaintenance `json:"maintenance_windows"`
	ProxyBindings         []ConfigProxy       `json:"proxy_bindings"`
	EscalationPolicies    []ConfigEscalation  `json:"escalation_policies"`
	Watchdog              ConfigWatchdog      `json:"watchdog"`
}

// ConfigAssignment resolves one monitor's dependencies and execution generation.
type ConfigAssignment struct {
	MonitorID            int64                    `json:"monitor_id"`
	Generation           Decimal                  `json:"generation"`
	Active               bool                     `json:"active"`
	Monitor              ConfigMonitor            `json:"monitor"`
	NotificationIDs      []int64                  `json:"notification_ids"`
	NotificationLinks    []ConfigNotificationLink `json:"notification_links"`
	MaintenanceIDs       []int64                  `json:"maintenance_ids"`
	ProxyBindingKey      *string                  `json:"proxy_binding_key"`
	ResourceBindings     []ResourceBinding        `json:"resource_bindings"`
	EscalationPolicyID   *int64                   `json:"escalation_policy_id"`
	RequiredCapabilities []string                 `json:"required_capabilities"`
}

// ConfigMonitor whitelists existing execution/template fields and their wire names.
type ConfigMonitor struct {
	Name                string          `json:"name"`
	Description         string          `json:"description"`
	Owner               string          `json:"owner"`
	EffectiveOwner      string          `json:"effective_owner"`
	Type                string          `json:"type"`
	Interval            int32           `json:"interval"`
	RetryInterval       int32           `json:"retry_interval"`
	MaxRetries          int32           `json:"max_retries"`
	Timeout             float64         `json:"timeout"`
	Config              json.RawMessage `json:"config"`
	AcceptedStatusCodes []string        `json:"accepted_statuscodes"`
	UpsideDown          bool            `json:"upside_down"`
	TLSIgnore           bool            `json:"tls_ignore"`
	CertExpiryNotify    bool            `json:"cert_expiry_notify"`
	ResendInterval      int32           `json:"resend_interval"`
	Tags                []ConfigTag     `json:"tags"`
}

// ConfigTag is resolved display metadata, without hub tag IDs or permissions.
type ConfigTag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ConfigNotificationLink preserves target visibility separately for each channel.
type ConfigNotificationLink struct {
	NotificationID int64 `json:"notification_id"`
	IncludeTarget  bool  `json:"include_target"`
}

// ConfigChannel carries confidential provider configuration and resolved template.
type ConfigChannel struct {
	ID            int64           `json:"id"`
	Version       Decimal         `json:"version"`
	Type          string          `json:"type"`
	Name          string          `json:"name"`
	Active        bool            `json:"active"`
	Config        json.RawMessage `json:"config"`
	TemplateID    *int64          `json:"template_id"`
	IncludeAckURL bool            `json:"include_ack_url"`
}

// ConfigTemplate carries the existing provider-specific renderer input.
type ConfigTemplate struct {
	ID            int64           `json:"id"`
	Provider      string          `json:"provider"`
	Name          string          `json:"name"`
	Version       Decimal         `json:"version"`
	TitleTemplate string          `json:"title_template"`
	BodyTemplate  string          `json:"body_template"`
	Config        json.RawMessage `json:"config"`
}

// ConfigMaintenance has explicit expanded applicability within this snapshot.
type ConfigMaintenance struct {
	ID         int64      `json:"id"`
	Active     bool       `json:"active"`
	Strategy   string     `json:"strategy"`
	StartDate  *Timestamp `json:"start_date"`
	EndDate    *Timestamp `json:"end_date"`
	CronExpr   string     `json:"cron_expr"`
	Duration   int32      `json:"duration"`
	Timezone   string     `json:"timezone"`
	MonitorIDs []int64    `json:"monitor_ids"`
}

// ConfigProxy contains a resolved outbound proxy and its confidential credentials.
type ConfigProxy struct {
	BindingKey string  `json:"binding_key"`
	Version    Decimal `json:"version"`
	Protocol   string  `json:"protocol"`
	Host       string  `json:"host"`
	Port       int32   `json:"port"`
	Auth       bool    `json:"auth"`
	Username   string  `json:"username"`
	Password   string  `json:"password"`
	Active     bool    `json:"active"`
}

// ConfigEscalation preserves an ordered policy, including empty/disabled policies.
type ConfigEscalation struct {
	ID      int64                  `json:"id"`
	Version Decimal                `json:"version"`
	Enabled bool                   `json:"enabled"`
	Steps   []ConfigEscalationStep `json:"steps"`
}

// ConfigEscalationStep keeps delay-after-previous-step semantics in seconds.
type ConfigEscalationStep struct {
	Step            int32   `json:"step"`
	DelaySeconds    int32   `json:"delay_seconds"`
	NotificationIDs []int64 `json:"notification_ids"`
}

// ConfigWatchdog describes source-owned connection alert settings, not a timer.
type ConfigWatchdog struct {
	Enabled             bool    `json:"enabled"`
	LostAfterSeconds    int32   `json:"lost_after_seconds"`
	RecoverAfterSeconds int32   `json:"recover_after_seconds"`
	NotificationIDs     []int64 `json:"notification_ids"`
	ResendInterval      int32   `json:"resend_interval"`
}

// DecodeConfigSnapshot validates shape, bounds, and all internal references.
// Extension objects still require checker/provider/template validation, and
// activation must authorize the target and atomically recheck durable revisions.
func DecodeConfigSnapshot(data []byte) (ConfigSnapshot, error) {
	return decodeCompleteConfigSnapshot(data, false)
}

// DecodeLocalConfigSnapshot validates the internal local snapshot dialect. Local
// push checks, direct Docker resources and opaque-token ack preferences are kept.
// It is not accepted by the remote protocol decoder or transfer assembler.
func DecodeLocalConfigSnapshot(data []byte) (ConfigSnapshot, error) {
	return decodeCompleteConfigSnapshot(data, true)
}

func decodeCompleteConfigSnapshot(data []byte, local bool) (ConfigSnapshot, error) {
	snapshot, err := decodeConfigSnapshotForTarget(data, local)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	if err := validateConfigReferences(snapshot); err != nil {
		return ConfigSnapshot{}, err
	}
	return snapshot, nil
}

func decodeConfigSnapshotForTarget(data []byte, local bool) (ConfigSnapshot, error) {
	var snapshot ConfigSnapshot
	if len(data) > MaxConfigSnapshotBytes {
		return snapshot, errors.New("configuration exceeds snapshot byte limit")
	}
	if err := validateJSON(data); err != nil {
		return snapshot, err
	}
	fields, err := decodeConfigFields(data, &snapshot, "schema_version hub_id probe_id revision created_at effective_at assignments notification_channels notification_templates maintenance_windows proxy_bindings escalation_policies watchdog", "")
	if err != nil {
		return snapshot, err
	}
	if err := requiredUUID(fields, "hub_id", &snapshot.HubID); err != nil {
		return snapshot, err
	}
	if local {
		if snapshot.ProbeID != domain.LocalProbeID {
			return snapshot, errors.New("local snapshot requires local target")
		}
	} else if err := requiredUUID(fields, "probe_id", &snapshot.ProbeID); err != nil {
		return snapshot, err
	}
	if snapshot.SchemaVersion != 1 || snapshot.Revision <= 0 {
		return snapshot, errors.New("unsupported config schema or nonpositive revision")
	}
	if snapshot.Assignments, err = decodeConfigList(fields["assignments"], MaxConfigAssignments, func(data []byte) (ConfigAssignment, error) { return decodeConfigAssignmentForTarget(data, local) }); err != nil {
		return snapshot, fmt.Errorf("assignments: %w", err)
	}
	if snapshot.NotificationChannels, err = decodeConfigList(fields["notification_channels"], MaxConfigDependencies, func(data []byte) (ConfigChannel, error) { return decodeConfigChannelForTarget(data, local) }); err != nil {
		return snapshot, fmt.Errorf("notification_channels: %w", err)
	}
	if snapshot.NotificationTemplates, err = decodeConfigList(fields["notification_templates"], MaxConfigDependencies, decodeConfigTemplate); err != nil {
		return snapshot, fmt.Errorf("notification_templates: %w", err)
	}
	if snapshot.MaintenanceWindows, err = decodeConfigList(fields["maintenance_windows"], MaxConfigDependencies, decodeConfigMaintenance); err != nil {
		return snapshot, fmt.Errorf("maintenance_windows: %w", err)
	}
	if snapshot.ProxyBindings, err = decodeConfigList(fields["proxy_bindings"], MaxConfigDependencies, decodeConfigProxy); err != nil {
		return snapshot, fmt.Errorf("proxy_bindings: %w", err)
	}
	if snapshot.EscalationPolicies, err = decodeConfigList(fields["escalation_policies"], MaxConfigDependencies, decodeConfigEscalation); err != nil {
		return snapshot, fmt.Errorf("escalation_policies: %w", err)
	}
	snapshot.Watchdog, err = decodeConfigWatchdog(fields["watchdog"])
	return snapshot, err
}

func decodeConfigAssignmentForTarget(data []byte, local bool) (ConfigAssignment, error) {
	var assignment ConfigAssignment
	fields, err := decodeConfigFields(data, &assignment, "monitor_id generation active monitor notification_ids notification_links maintenance_ids resource_bindings required_capabilities", "proxy_binding_key escalation_policy_id")
	if err != nil {
		return assignment, err
	}
	if assignment.MonitorID <= 0 || assignment.Generation <= 0 || !validConfigIDs(assignment.NotificationIDs, MaxConfigDependencies) || !validConfigIDs(assignment.MaintenanceIDs, MaxConfigDependencies) {
		return assignment, errors.New("invalid assignment identity or reference IDs")
	}
	if assignment.ProxyBindingKey != nil && !validBindingKey(*assignment.ProxyBindingKey) || assignment.EscalationPolicyID != nil && *assignment.EscalationPolicyID <= 0 {
		return assignment, errors.New("invalid assignment proxy/policy reference")
	}
	if assignment.Monitor, err = decodeConfigMonitorForTarget(fields["monitor"], local); err != nil {
		return assignment, err
	}
	if assignment.NotificationLinks, err = decodeConfigList(fields["notification_links"], MaxConfigDependencies, decodeConfigNotificationLink); err != nil {
		return assignment, err
	}
	if assignment.ResourceBindings, err = decodeConfigList(fields["resource_bindings"], 1, decodeResourceBinding); err != nil {
		return assignment, err
	}
	if local && len(assignment.ResourceBindings) != 0 || !local && (assignment.Monitor.Type == "docker") != (len(assignment.ResourceBindings) == 1) {
		return assignment, errors.New("only Docker assignments require one local resource binding")
	}
	if err := validateCapabilities(assignment.RequiredCapabilities); err != nil {
		return assignment, err
	}
	if !containsCapability(assignment.RequiredCapabilities, "checker."+assignment.Monitor.Type+".v1") {
		return assignment, errors.New("assignment is missing its checker capability")
	}
	return assignment, nil
}

func decodeConfigMonitorForTarget(data []byte, local bool) (ConfigMonitor, error) {
	var monitor ConfigMonitor
	fields, err := decodeConfigFields(data, &monitor, "name description owner effective_owner type interval retry_interval max_retries timeout config accepted_statuscodes upside_down tls_ignore cert_expiry_notify resend_interval tags", "")
	if err != nil {
		return monitor, err
	}
	if !validConfigName(monitor.Name) || len(monitor.Description) > 16<<10 || len(monitor.Owner) > 16<<10 || len(monitor.EffectiveOwner) > 16<<10 || (!pullMonitorType(monitor.Type) && !(local && monitor.Type == "push")) {
		return monitor, errors.New("invalid monitor metadata or unsupported remote type")
	}
	if monitor.Interval <= 0 || monitor.RetryInterval < 0 || monitor.MaxRetries < 0 || monitor.Timeout <= 0 || monitor.Timeout > math.MaxInt32 || !validConfigMinutes(monitor.ResendInterval) {
		return monitor, errors.New("invalid monitor timing or retry bounds")
	}
	if !validConfigExtension(monitor.Config, MaxConfigObjectBytes) || len(monitor.AcceptedStatusCodes) > 64 {
		return monitor, errors.New("invalid checker object or status-code bounds")
	}
	for _, code := range monitor.AcceptedStatusCodes {
		if len(code) == 0 || len(code) > 64 {
			return monitor, errors.New("invalid status-code string length")
		}
	}
	monitor.Tags, err = decodeConfigList(fields["tags"], MaxConfigTags, decodeConfigTag)
	if err != nil {
		return monitor, err
	}
	seen := make(map[string]bool, len(monitor.Tags))
	for _, tag := range monitor.Tags {
		if seen[tag.Name] {
			return monitor, errors.New("duplicate monitor tag name")
		}
		seen[tag.Name] = true
	}
	return monitor, nil
}

func decodeConfigTag(data []byte) (ConfigTag, error) {
	var tag ConfigTag
	if _, err := decodeConfigFields(data, &tag, "name value", ""); err != nil {
		return tag, err
	}
	if !validConfigName(tag.Name) || len(tag.Value) > MaxMessageBytes {
		return tag, errors.New("invalid tag metadata")
	}
	return tag, nil
}

func decodeConfigNotificationLink(data []byte) (ConfigNotificationLink, error) {
	var link ConfigNotificationLink
	if _, err := decodeConfigFields(data, &link, "notification_id include_target", ""); err != nil {
		return link, err
	}
	if link.NotificationID <= 0 {
		return link, errors.New("invalid notification link ID")
	}
	return link, nil
}

func decodeConfigFields(data []byte, target any, nonnull, nullableFields string) (map[string]json.RawMessage, error) {
	fields, err := objectFields(data)
	if err != nil {
		return nil, err
	}
	for _, key := range strings.Fields(nonnull) {
		value, exists := fields[key]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("%s is required and cannot be null", key)
		}
	}
	for _, key := range strings.Fields(nullableFields) {
		if _, exists := fields[key]; !exists {
			return nil, fmt.Errorf("%s is required; use null when unavailable", key)
		}
	}
	// Decode nested objects/collections only in their bounded schema decoders.
	// Otherwise a tiny array of nulls could allocate millions of large DTOs
	// before its collection count is checked.
	selected := make(map[string]json.RawMessage)
	for _, key := range strings.Fields(nonnull + " " + nullableFields) {
		switch key {
		case "assignments", "notification_channels", "notification_templates", "maintenance_windows", "proxy_bindings", "escalation_policies", "watchdog", "monitor", "notification_links", "resource_bindings", "tags", "steps":
			continue
		}
		value := fields[key]
		if trimmed := bytes.TrimSpace(value); len(trimmed) > 0 && trimmed[0] == '[' {
			if _, err := decodeConfigList(value, MaxConfigAssignments, func([]byte) (struct{}, error) { return struct{}{}, nil }); err != nil {
				return nil, err
			}
		}
		selected[key] = value
	}
	encoded, err := json.Marshal(selected)
	if err != nil {
		return nil, fmt.Errorf("decoding config fields: %w", err)
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return nil, fmt.Errorf("decoding config object: %w", err)
	}
	return fields, nil
}

func decodeConfigList[T any](data []byte, limit int, decode func([]byte) (T, error)) ([]T, error) {
	reader := json.NewDecoder(bytes.NewReader(data))
	token, err := reader.Token()
	if err != nil || token != json.Delim('[') {
		return nil, errors.New("config collection must be an array")
	}
	result := make([]T, 0)
	for reader.More() {
		if len(result) >= limit {
			return nil, errors.New("config collection exceeds count limit")
		}
		var entry json.RawMessage
		if err := reader.Decode(&entry); err != nil {
			return nil, fmt.Errorf("decoding config entry: %w", err)
		}
		if len(entry) > MaxConfigEntryBytes {
			return nil, errors.New("config entry exceeds byte limit")
		}
		value, err := decode(entry)
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", len(result), err)
		}
		result = append(result, value)
	}
	if token, err := reader.Token(); err != nil || token != json.Delim(']') {
		return nil, errors.New("invalid config collection end")
	}
	if _, err := reader.Token(); err != io.EOF {
		return nil, errors.New("trailing config collection data")
	}
	return result, nil
}

func validConfigExtension(data []byte, limit int) bool {
	if len(data) > limit {
		return false
	}
	_, err := objectFields(data)
	return err == nil
}

func validConfigIDs(ids []int64, limit int) bool {
	if ids == nil || len(ids) > limit {
		return false
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func validConfigName(name string) bool {
	return strings.TrimSpace(name) != "" && len(name) <= 255
}

func validConfigMinutes(value int32) bool {
	return value >= 0 && int64(value) <= maxConfigMinutes
}

func containsCapability(capabilities []string, required string) bool {
	for _, capability := range capabilities {
		if capability == required {
			return true
		}
	}
	return false
}

func pullMonitorType(kind string) bool {
	switch kind {
	case "http", "tcp", "ping", "dns", "websocket", "docker", "mqtt", "rabbitmq", "grpc", "snmp", "database", "s3":
		return true
	default:
		return false
	}
}
