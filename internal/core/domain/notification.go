package domain

import "time"

// Notification represents a notification provider configuration.
type Notification struct {
	ID        int64
	UserID    int64
	Name      string
	Type      string // "telegram", "discord", "slack", ...
	Active    bool
	IsDefault bool
	// IncludeAckURL controls whether DOWN messages on this channel carry the
	// public acknowledgement deep-link. False is the default: Discord gets an
	// Acknowledge button when this is on; other providers append a text link.
	IncludeAckURL bool
	TemplateID    *int64
	Config        map[string]any // per-provider config (JSONB in DB)
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// DefaultIncludeAckURL is the create-time default when the field is omitted.
const DefaultIncludeAckURL = false

// MonitorNotification represents the link between a monitor and a notification.
type MonitorNotification struct {
	ID             int64
	MonitorID      int64
	NotificationID int64
}

// GroupNotification represents the link between a monitor group (folder) and a
// notification.
//
// A group is an alerting entity in its own right: it alerts on its OWN derived
// status — the rollup its Condition produces — and never by inheriting the link
// down to the monitors inside it. Attaching a notification to a group therefore
// says "tell me when this folder as a whole trips", not "tell me about every
// monitor in this folder".
type GroupNotification struct {
	ID             int64
	GroupID        int64
	NotificationID int64
}

// NotificationSentHistory tracks the last time a notification was sent for a monitor.
type NotificationSentHistory struct {
	ID             int64
	NotificationID int64
	MonitorID      int64
	LastSentAt     time.Time
}

// Alert event kinds. Empty EventKind is treated as status_change for
// backward compatibility with existing callers and tests.
const (
	AlertEventStatusChange      = "status_change"
	AlertEventCertificateExpiry = "certificate_expiry"
	AlertEventCapacityCondition = "capacity_condition"

	AlertScopeMonitor = "monitor"
	AlertScopeGroup   = "group"
)

// AlertContext contains the data needed to render an alert notification.
type AlertContext struct {
	AlertScope              string
	MonitorID               int64
	MonitorName             string
	MonitorType             string
	MonitorTarget           string
	MonitorDescription      string
	MonitorOwner            string
	GroupID                 int64
	GroupName               string
	GroupDescription        string
	GroupOwner              string
	GroupCondition          GroupCondition
	GroupThreshold          int
	GroupThresholdIsPercent bool
	Status                  Status
	PreviousStatus          Status
	Message                 string
	Duration                time.Duration
	StartedAt               time.Time
	CheckOutput             string
	Tags                    map[string]string

	// EventKind distinguishes status transitions from auxiliary alerts. Senders
	// MUST render certificate_expiry and capacity_condition with event-specific
	// copy, not as fake DOWN/UP/PENDING status changes.
	EventKind string

	// Certificate-expiry fields (set only when EventKind is certificate_expiry).
	CertThreshold     int        // 30, 14, or 7
	CertDaysRemaining int        // whole days remaining at evaluation time
	CertIssuer        string     // certificate issuer DN when known
	CertNotAfter      *time.Time // exact NotAfter instant when known

	// Capacity-condition fields are set only for capacity_condition events.
	ConditionKind          string
	ConditionState         ConditionState
	ConditionPreviousState ConditionState
	ConditionUsed          *float64
	ConditionLimit         *float64
	ConditionPercent       *float64
	ConditionThreshold     *float64
	ConditionUnit          string
	ConditionResource      string
	ConditionScope         string
	ConditionSource        string
	ConditionObservedAt    *time.Time

	// AckURL is an absolute deep-link that acknowledges the open outage alert
	// without a session (F2.2). Empty when PUBLIC_URL is unset or the event is
	// not a DOWN status change. Senders may surface it; NotificationService also
	// appends it to Message so every provider carries the link by default.
	AckURL string

	// TemplateTitle and TemplateBody are resolved by NotificationService for the
	// notification currently being sent. Empty values preserve the provider's
	// built-in layout.
	TemplateTitle  string
	TemplateBody   string
	TemplateConfig map[string]any
}
