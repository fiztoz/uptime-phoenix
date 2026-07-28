// Package ws provides the WebSocket hub adapter.
package ws

// WebSocket event type constants for real-time communication.
const (
	EventHeartbeat       = "heartbeat"
	EventStatusChange    = "status.change"
	EventMonitorUpdate   = "monitor.update"
	EventMonitorDelete   = "monitor.delete"
	EventMonitorList     = "monitor.list"
	EventIncidentCreate  = "incident.create"
	EventIncidentResolve = "incident.resolve"
	EventStatsUpdate     = "stats.update"
)
