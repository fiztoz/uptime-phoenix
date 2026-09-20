package domain

// ProbeWatchdogDeliveryEligible verifies the immutable source transition behind
// a send. ACK/recovery supersede DOWN; ACK does not invalidate its later UP. A
// completed recovery can remain deliverable after another outage has started.
// Configuration/channel and current DB ownership checks belong to its caller.
func ProbeWatchdogDeliveryEligible(item QueuedDelivery, incident *RegionalIncident) bool {
	if incident == nil || item.EventKind != DeliveryEventProbeConnection || item.MonitorID != 0 || item.AssignmentGeneration != 0 ||
		incident.Scope != IncidentScopeProbeConnection || incident.SubjectKind != IncidentSubjectWatchdog || incident.MonitorID != 0 || incident.AssignmentGeneration != 0 ||
		item.ProbeID != incident.ProbeID || item.SourceAlertID != incident.SourceAlertID || item.SourceTransitionVersion != incident.TransitionVersion ||
		item.StartedAt.UTC().UnixMicro() != incident.StartedAt.UTC().UnixMicro() {
		return false
	}
	switch item.CheckStatus {
	case StatusDown:
		return incident.Status == AlertStatusFiring && incident.AckedAt == nil && incident.ResolvedAt == nil && item.IncidentStatus == AlertStatusFiring && item.ResolvedAt == nil
	case StatusUp:
		return incident.Status == AlertStatusResolved && item.IncidentStatus == AlertStatusResolved && item.ResolvedAt != nil && incident.ResolvedAt != nil && item.ResolvedAt.UTC().UnixMicro() == incident.ResolvedAt.UTC().UnixMicro()
	default:
		return false
	}
}
