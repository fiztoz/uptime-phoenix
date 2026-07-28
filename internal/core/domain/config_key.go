package domain

import "time"

// Config resource type constants used in config_keys.resource_type.
const (
	ConfigResourceTag               = "tag"
	ConfigResourceProxy             = "proxy"
	ConfigResourceNotification      = "notification"
	ConfigResourceMonitorGroup      = "monitor_group"
	ConfigResourceMonitor           = "monitor"
	ConfigResourceStatusPage        = "status_page"
	ConfigResourceMaintenanceWindow = "maintenance_window"
)

// ConfigKey maps an operator-defined stable key to a persisted resource ID.
//
// Keys are the identity used by declarative config-as-code documents. They are
// unique per resource type. A resource without a ConfigKey is owned by the
// admin UI / ad-hoc API and is never pruned by config apply.
type ConfigKey struct {
	ID           int64
	ResourceType string
	KeyName      string
	ResourceID   int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
