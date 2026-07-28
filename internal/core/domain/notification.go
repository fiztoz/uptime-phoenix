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
	Config    map[string]any // per-provider config (JSONB in DB)
	CreatedAt time.Time
	UpdatedAt time.Time
}

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
)

// AlertContext contains the data needed to render an alert notification.
type AlertContext struct {
	MonitorID      int64
	MonitorName    string
	MonitorType    string
	MonitorTarget  string
	Status         Status
	PreviousStatus Status
	Message        string
	Duration       time.Duration
	StartedAt      time.Time
	CheckOutput    string
	Tags           map[string]string

	// EventKind distinguishes status transitions from certificate-expiry alerts.
	// Senders MUST render certificate_expiry with cert-specific copy, not as
	// a fake DOWN/UP/PENDING status change.
	EventKind string

	// Certificate-expiry fields (set only when EventKind is certificate_expiry).
	CertThreshold     int        // 30, 14, or 7
	CertDaysRemaining int        // whole days remaining at evaluation time
	CertIssuer        string     // certificate issuer DN when known
	CertNotAfter      *time.Time // exact NotAfter instant when known

	// AckURL is an absolute deep-link that acknowledges the open outage alert
	// without a session (F2.2). Empty when PUBLIC_URL is unset or the event is
	// not a DOWN status change. Senders may surface it; NotificationService also
	// appends it to Message so every provider carries the link by default.
	AckURL string
}
