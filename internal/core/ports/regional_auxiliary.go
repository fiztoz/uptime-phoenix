package ports

// RegionalTLSInfoRepository creates a repository view for one assignment.
// Binding is storage scoping, not authorization to execute or deliver alerts.
type RegionalTLSInfoRepository interface {
	ForAssignment(probeID string, generation int64) (TLSInfoRepository, error)
}

// RegionalConditionRepository creates a repository view whose reads, writes,
// and deletes are confined to one probe and assignment generation.
type RegionalConditionRepository interface {
	ForAssignment(probeID string, generation int64) (MonitorConditionRepository, error)
}
