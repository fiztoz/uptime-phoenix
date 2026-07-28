package services

// Config API constants (F5 Sprint 14).
const (
	// ConfigAPIVersion is the only accepted document version.
	ConfigAPIVersion = "phoenix.dev/v1"
	// ConfigKind is the document kind.
	ConfigKind = "Config"
	// ConfigSecretRedacted is written on export and treated as "preserve" on apply.
	ConfigSecretRedacted = "__REDACTED__"
)

// ConfigDocument is the versioned declarative config shape (YAML or JSON).
//
// It never embeds domain.* types. Relationships use operator keys, not DB IDs.
// See docs/F5-S14-CONFIG-AS-CODE-CONTRACTS.md.
type ConfigDocument struct {
	APIVersion string     `json:"apiVersion" yaml:"apiVersion"`
	Kind       string     `json:"kind" yaml:"kind"`
	Spec       ConfigSpec `json:"spec" yaml:"spec"`
}

// ConfigSpec holds the managed resource lists.
type ConfigSpec struct {
	Tags                 []ConfigTag                 `json:"tags,omitempty" yaml:"tags,omitempty"`
	Proxies              []ConfigProxy               `json:"proxies,omitempty" yaml:"proxies,omitempty"`
	Notifications        []ConfigNotification        `json:"notifications,omitempty" yaml:"notifications,omitempty"`
	MonitorGroups        []ConfigMonitorGroup        `json:"monitor_groups,omitempty" yaml:"monitor_groups,omitempty"`
	Monitors             []ConfigMonitor             `json:"monitors,omitempty" yaml:"monitors,omitempty"`
	MonitorTags          []ConfigMonitorTag          `json:"monitor_tags,omitempty" yaml:"monitor_tags,omitempty"`
	MonitorNotifications []ConfigMonitorNotification `json:"monitor_notifications,omitempty" yaml:"monitor_notifications,omitempty"`
	GroupNotifications   []ConfigGroupNotification   `json:"group_notifications,omitempty" yaml:"group_notifications,omitempty"`
	StatusPages          []ConfigStatusPage          `json:"status_pages,omitempty" yaml:"status_pages,omitempty"`
	StatusPageMonitors   []ConfigStatusPageMonitor   `json:"status_page_monitors,omitempty" yaml:"status_page_monitors,omitempty"`
	MaintenanceWindows   []ConfigMaintenance         `json:"maintenance_windows,omitempty" yaml:"maintenance_windows,omitempty"`
	MaintenanceMonitors  []ConfigMaintenanceMonitor  `json:"maintenance_monitors,omitempty" yaml:"maintenance_monitors,omitempty"`
}

// ConfigTag is a tag declaration.
type ConfigTag struct {
	Key   string `json:"key" yaml:"key"`
	Name  string `json:"name" yaml:"name"`
	Color string `json:"color,omitempty" yaml:"color,omitempty"`
}

// ConfigProxy is a proxy declaration.
type ConfigProxy struct {
	Key       string `json:"key" yaml:"key"`
	Protocol  string `json:"protocol" yaml:"protocol"`
	Host      string `json:"host" yaml:"host"`
	Port      int    `json:"port" yaml:"port"`
	Auth      bool   `json:"auth,omitempty" yaml:"auth,omitempty"`
	Username  string `json:"username,omitempty" yaml:"username,omitempty"`
	Password  string `json:"password,omitempty" yaml:"password,omitempty"`
	Active    *bool  `json:"active,omitempty" yaml:"active,omitempty"`
	IsDefault bool   `json:"is_default,omitempty" yaml:"is_default,omitempty"`
}

// ConfigNotification is a notification channel declaration.
type ConfigNotification struct {
	Key       string         `json:"key" yaml:"key"`
	Name      string         `json:"name" yaml:"name"`
	Type      string         `json:"type" yaml:"type"`
	Active    *bool          `json:"active,omitempty" yaml:"active,omitempty"`
	IsDefault bool           `json:"is_default,omitempty" yaml:"is_default,omitempty"`
	Config    map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
}

// ConfigMonitorGroup is a folder declaration.
type ConfigMonitorGroup struct {
	Key                string `json:"key" yaml:"key"`
	Name               string `json:"name" yaml:"name"`
	Description        string `json:"description,omitempty" yaml:"description,omitempty"`
	Parent             string `json:"parent,omitempty" yaml:"parent,omitempty"` // key of parent group
	Condition          string `json:"condition,omitempty" yaml:"condition,omitempty"`
	Threshold          int    `json:"threshold,omitempty" yaml:"threshold,omitempty"`
	ThresholdIsPercent bool   `json:"threshold_is_percent,omitempty" yaml:"threshold_is_percent,omitempty"`
	Weight             int    `json:"weight,omitempty" yaml:"weight,omitempty"`
	Collapsed          bool   `json:"collapsed,omitempty" yaml:"collapsed,omitempty"`
}

// ConfigMonitor is a monitor declaration.
type ConfigMonitor struct {
	Key                 string         `json:"key" yaml:"key"`
	Name                string         `json:"name" yaml:"name"`
	Description         string         `json:"description,omitempty" yaml:"description,omitempty"`
	Type                string         `json:"type" yaml:"type"`
	Active              *bool          `json:"active,omitempty" yaml:"active,omitempty"`
	Interval            int            `json:"interval,omitempty" yaml:"interval,omitempty"`
	RetryInterval       int            `json:"retry_interval,omitempty" yaml:"retry_interval,omitempty"`
	MaxRetries          int            `json:"max_retries,omitempty" yaml:"max_retries,omitempty"`
	Timeout             float64        `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Config              map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
	AcceptedStatusCodes []string       `json:"accepted_statuscodes,omitempty" yaml:"accepted_statuscodes,omitempty"`
	Proxy               string         `json:"proxy,omitempty" yaml:"proxy,omitempty"` // key
	Group               string         `json:"group,omitempty" yaml:"group,omitempty"` // key
	UpsideDown          bool           `json:"upside_down,omitempty" yaml:"upside_down,omitempty"`
	ResendInterval      int            `json:"resend_interval,omitempty" yaml:"resend_interval,omitempty"`
	Weight              int            `json:"weight,omitempty" yaml:"weight,omitempty"`
	TLSIgnore           bool           `json:"tls_ignore,omitempty" yaml:"tls_ignore,omitempty"`
	CertExpiryNotify    bool           `json:"cert_expiry_notify,omitempty" yaml:"cert_expiry_notify,omitempty"`
}

// ConfigMonitorTag links a monitor key to a tag key.
type ConfigMonitorTag struct {
	Monitor string `json:"monitor" yaml:"monitor"`
	Tag     string `json:"tag" yaml:"tag"`
	Value   string `json:"value,omitempty" yaml:"value,omitempty"`
}

// ConfigMonitorNotification links a monitor key to a notification key.
type ConfigMonitorNotification struct {
	Monitor      string `json:"monitor" yaml:"monitor"`
	Notification string `json:"notification" yaml:"notification"`
}

// ConfigGroupNotification links a group key to a notification key.
type ConfigGroupNotification struct {
	Group        string `json:"group" yaml:"group"`
	Notification string `json:"notification" yaml:"notification"`
}

// ConfigStatusPage is a status page declaration.
type ConfigStatusPage struct {
	Key                  string `json:"key" yaml:"key"`
	Slug                 string `json:"slug" yaml:"slug"`
	Title                string `json:"title" yaml:"title"`
	Description          string `json:"description,omitempty" yaml:"description,omitempty"`
	Icon                 string `json:"icon,omitempty" yaml:"icon,omitempty"`
	Theme                string `json:"theme,omitempty" yaml:"theme,omitempty"`
	Published            *bool  `json:"published,omitempty" yaml:"published,omitempty"`
	CustomDomain         string `json:"custom_domain,omitempty" yaml:"custom_domain,omitempty"`
	AccessCode           string `json:"access_code,omitempty" yaml:"access_code,omitempty"` // write-only; redacted on export
	FooterText           string `json:"footer_text,omitempty" yaml:"footer_text,omitempty"`
	CustomCSS            string `json:"custom_css,omitempty" yaml:"custom_css,omitempty"`
	DashboardStyle       string `json:"dashboard_style,omitempty" yaml:"dashboard_style,omitempty"`
	ShowTags             bool   `json:"show_tags,omitempty" yaml:"show_tags,omitempty"`
	AutoResolveIncidents bool   `json:"auto_resolve_incidents,omitempty" yaml:"auto_resolve_incidents,omitempty"`
	// ShowPoweredBy defaults true when omitted on apply for new pages (handler-level).
	// In config-as-code export it is always explicit.
	ShowPoweredBy *bool    `json:"show_powered_by,omitempty" yaml:"show_powered_by,omitempty"`
	Favicon       string   `json:"favicon,omitempty" yaml:"favicon,omitempty"`
	SLATarget     *float64 `json:"sla_target,omitempty" yaml:"sla_target,omitempty"`
}

// ConfigStatusPageMonitor links a status page key to a monitor key.
type ConfigStatusPageMonitor struct {
	StatusPage   string `json:"status_page" yaml:"status_page"`
	Monitor      string `json:"monitor" yaml:"monitor"`
	DisplayOrder int    `json:"display_order,omitempty" yaml:"display_order,omitempty"`
}

// ConfigMaintenance is a maintenance window declaration.
type ConfigMaintenance struct {
	Key         string `json:"key" yaml:"key"`
	Title       string `json:"title" yaml:"title"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Active      *bool  `json:"active,omitempty" yaml:"active,omitempty"`
	Strategy    string `json:"strategy" yaml:"strategy"`
	StartDate   string `json:"start_date,omitempty" yaml:"start_date,omitempty"` // RFC3339
	EndDate     string `json:"end_date,omitempty" yaml:"end_date,omitempty"`
	CronExpr    string `json:"cron_expr,omitempty" yaml:"cron_expr,omitempty"`
	Duration    int    `json:"duration,omitempty" yaml:"duration,omitempty"`
	Timezone    string `json:"timezone,omitempty" yaml:"timezone,omitempty"`
}

// ConfigMaintenanceMonitor links a maintenance key to a monitor key.
type ConfigMaintenanceMonitor struct {
	Maintenance string `json:"maintenance" yaml:"maintenance"`
	Monitor     string `json:"monitor" yaml:"monitor"`
}

// ConfigChangeAction is create | update | delete | unchanged.
type ConfigChangeAction string

const (
	ConfigActionCreate    ConfigChangeAction = "create"
	ConfigActionUpdate    ConfigChangeAction = "update"
	ConfigActionDelete    ConfigChangeAction = "delete"
	ConfigActionUnchanged ConfigChangeAction = "unchanged"
)

// ConfigChange is one planned or applied mutation.
type ConfigChange struct {
	Kind   string             `json:"kind"`
	Key    string             `json:"key"`
	Action ConfigChangeAction `json:"action"`
	Detail string             `json:"detail,omitempty"`
}

// ConfigPlan is the dry-run result of comparing a document to live state.
type ConfigPlan struct {
	Valid     bool           `json:"valid"`
	Errors    []string       `json:"errors,omitempty"`
	Changes   []ConfigChange `json:"changes"`
	Creates   int            `json:"creates"`
	Updates   int            `json:"updates"`
	Deletes   int            `json:"deletes"`
	Unchanged int            `json:"unchanged"`
}

// ConfigApplyResult is returned after a successful apply.
type ConfigApplyResult struct {
	Plan      *ConfigPlan    `json:"plan"`
	Applied   []ConfigChange `json:"applied"`
	Creates   int            `json:"creates"`
	Updates   int            `json:"updates"`
	Deletes   int            `json:"deletes"`
	Unchanged int            `json:"unchanged"`
}

// ConfigApplyOptions controls apply/plan behavior.
type ConfigApplyOptions struct {
	// Prune deletes keyed resources missing from the document.
	Prune bool
}
