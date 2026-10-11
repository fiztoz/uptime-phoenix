package domain

// AcknowledgedProbeDeliveryConfig is a nonsecret projection of an authenticated,
// receipt-selected document. It proves document acceptance, never runtime mode
// application, current lease ownership or permission to send/suppress alerts.
type AcknowledgedProbeDeliveryConfig struct {
	Target      ProbeConfigTarget
	Revision    int64
	Assignments []AcknowledgedAssignmentDelivery
}

// AcknowledgedAssignmentDelivery retains only assignment identity and the desired
// availability delivery mode in the exact acknowledged configuration document.
type AcknowledgedAssignmentDelivery struct {
	MonitorID     int64
	Generation    int64
	AlertDelivery AlertDelivery
}
