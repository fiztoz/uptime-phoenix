package services

import (
	"sort"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// edgeRegionalApplications describes the mode this runtime actually implements,
// never the desired mode carried by the document. Legacy assignments remain
// unbound. These source-local facts do not advertise aggregate application support.
func edgeRegionalApplications(config *domain.EdgeResolvedConfig, streamID string) ([]domain.ProbeDeliveryModeApplication, error) {
	if config == nil {
		return nil, domain.ErrValidation
	}
	facts := make([]domain.ProbeDeliveryModeApplication, 0, len(config.Assignments))
	for _, assignment := range config.Assignments {
		if assignment.AssignmentRevision == 0 {
			continue
		}
		if assignment.Monitor == nil {
			return nil, domain.ErrValidation
		}
		fact := domain.ProbeDeliveryModeApplication{
			ProbeConfigTarget: config.Metadata.ProbeConfigTarget,
			MonitorID:         assignment.Monitor.ID, AssignmentRevision: assignment.AssignmentRevision,
			Generation: assignment.Generation, ConfigRevision: config.Metadata.Revision,
			SHA256: config.Metadata.SHA256, StreamID: streamID, AlertDelivery: domain.AlertDeliveryRegional,
		}
		if !domain.ValidProbeDeliveryModeApplication(fact) {
			return nil, domain.ErrValidation
		}
		facts = append(facts, fact)
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].MonitorID < facts[j].MonitorID })
	return facts, nil
}

// validateEdgeRegionalApplications checks present source facts against exact
// authenticated bytes. Missing legacy/pre-upgrade facts are unreported, not a
// synthesized application. Current source stream selection belongs to storage.
func validateEdgeRegionalApplications(config *domain.EdgeResolvedConfig, facts []domain.ProbeDeliveryModeApplication) error {
	byMonitor := make(map[int64]domain.EdgeResolvedAssignment, len(config.Assignments))
	for _, assignment := range config.Assignments {
		if assignment.Monitor == nil {
			return domain.ErrValidation
		}
		byMonitor[assignment.Monitor.ID] = assignment
	}
	seen := make(map[int64]bool, len(facts))
	for _, fact := range facts {
		assignment, exists := byMonitor[fact.MonitorID]
		if !exists || seen[fact.MonitorID] || !domain.ValidProbeDeliveryModeApplication(fact) ||
			fact.ProbeConfigTarget != config.Metadata.ProbeConfigTarget || fact.ConfigRevision != config.Metadata.Revision ||
			fact.SHA256 != config.Metadata.SHA256 || fact.AlertDelivery != domain.AlertDeliveryRegional ||
			fact.AssignmentRevision != assignment.AssignmentRevision || fact.Generation != assignment.Generation {
			return domain.ErrValidation
		}
		seen[fact.MonitorID] = true
	}
	return nil
}
