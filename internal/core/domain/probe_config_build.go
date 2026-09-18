package domain

import "time"

// ProbeConfigAssignment is confidential resolved input to the snapshot encoder.
// Monitor carries execution settings; only the encoder's whitelist may leave it.
type ProbeConfigAssignment struct {
	Monitor            *Monitor
	Generation         int64
	EffectiveOwner     string
	Tags               []ProbeConfigTag
	NotificationLinks  []MonitorNotification
	MaintenanceIDs     []int64
	EscalationPolicyID *int64
}

// ProbeConfigTag is resolved template metadata.
type ProbeConfigTag struct {
	Name  string
	Value string
}

// ProbeConfigMaintenance retains explicit applicability. An empty MonitorIDs
// list covers nothing, matching the existing maintenance suppression path.
type ProbeConfigMaintenance struct {
	Window     *MaintenanceWindow
	MonitorIDs []int64
}

// LocalProbeConfigSource is one consistent hub database read. It contains secrets
// and must never be logged or exposed through an HTTP view. Ancestor policies
// may include overridden dependencies; resolution removes those before encoding.
type LocalProbeConfigSource struct {
	Probe           Probe
	Assignments     []ProbeConfigAssignment
	Groups          map[int64]*MonitorGroup
	MonitorPolicies map[int64]int64
	GroupPolicies   map[int64]int64
	Policies        map[int64]*EscalationPolicy
	Notifications   map[int64]*Notification
	Templates       map[int64]*NotificationTemplate
	Proxies         map[int64]*Proxy
	Maintenance     []ProbeConfigMaintenance
}

// LocalProbeConfigDefinition is a complete local dependency closure, not an
// activated revision. The transport adapter supplies explicit JSON DTOs.
type LocalProbeConfigDefinition struct {
	Target        ProbeConfigTarget
	Revision      int64
	CreatedAt     time.Time
	EffectiveAt   time.Time
	Assignments   []ProbeConfigAssignment
	Policies      []*EscalationPolicy
	Notifications []*Notification
	Templates     []*NotificationTemplate
	Proxies       []*Proxy
	Maintenance   []ProbeConfigMaintenance
}
