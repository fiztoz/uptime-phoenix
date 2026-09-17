package domain

// NotificationThrottleKey identifies availability resend state for one assignment.
// It is not an incident identity or authority to execute a check or send an alert.
type NotificationThrottleKey struct {
	MonitorID            int64
	ProbeID              string
	AssignmentGeneration int64
}
