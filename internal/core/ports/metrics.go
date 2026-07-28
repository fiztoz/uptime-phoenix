package ports

import "context"

// MetricsExporter defines the interface for exporting application metrics.
type MetricsExporter interface {
	SetMonitorStatus(monitorID int64, monitorName, monitorType string, status float64)
	SetMonitorLatency(monitorID int64, monitorName string, latencyMs float64)
	IncHeartbeat(monitorID int64, status string)
	IncNotificationSent(provider, status string)
	SetWSConnectionsActive(count float64)
	SetMonitorsActive(count float64)
	Handler() (any, error)
	Shutdown(ctx context.Context) error
}
