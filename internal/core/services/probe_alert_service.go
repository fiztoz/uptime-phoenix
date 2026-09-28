package services

import (
	"context"
	"fmt"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeAlertEntry contains safe attribution for one monitor's source incident.
type ProbeAlertEntry struct {
	Incident domain.RegionalIncident
	Name     string
	Location string
}

// ProbeAlertService applies existing monitor visibility to regional incidents
// and persists commands without claiming suppression before a remote receipt.
type ProbeAlertService struct {
	access       *AccessService
	incidents    ports.ProbeIncidentRepository
	registry     ports.ProbeRegistryRepository
	installation ports.ProbeInstallationRepository
	commands     *ProbeCommandService
	receipts     ports.ProbeCommandRepository
}

// NewProbeAlertService binds the existing source-incident and command lifecycles.
func NewProbeAlertService(access *AccessService, incidents ports.ProbeIncidentRepository, registry ports.ProbeRegistryRepository, installation ports.ProbeInstallationRepository, commands *ProbeCommandService, receipts ports.ProbeCommandRepository) *ProbeAlertService {
	return &ProbeAlertService{access: access, incidents: incidents, registry: registry, installation: installation, commands: commands, receipts: receipts}
}

func (s *ProbeAlertService) authorize(ctx context.Context, userID, monitorID int64) error {
	if s == nil || s.access == nil || s.incidents == nil || userID <= 0 || monitorID <= 0 {
		return ports.ErrNotFound
	}
	allowed, err := s.access.CanViewMonitor(ctx, userID, monitorID)
	if err != nil {
		return err
	}
	if !allowed {
		return ports.ErrNotFound
	}
	return nil
}

// List returns only the requested visible monitor's remote incidents.
func (s *ProbeAlertService) List(ctx context.Context, userID, monitorID int64) ([]ProbeAlertEntry, error) {
	if err := s.authorize(ctx, userID, monitorID); err != nil {
		return nil, err
	}
	rows, err := s.incidents.ListIncidentsByMonitor(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	out := make([]ProbeAlertEntry, 0, len(rows))
	labels := make(map[string]*domain.Probe)
	for _, row := range rows {
		if row.Scope != domain.IncidentScopeRegional || row.ProbeID == domain.LocalProbeID {
			continue
		}
		label := labels[row.ProbeID]
		if label == nil {
			label, err = s.registry.GetByID(ctx, row.ProbeID)
			if err != nil {
				return nil, err
			}
			labels[row.ProbeID] = label
		}
		out = append(out, ProbeAlertEntry{Incident: row, Name: label.Name, Location: label.Location})
	}
	return out, nil
}

func (s *ProbeAlertService) incident(ctx context.Context, userID, monitorID int64, alertID string) (*domain.RegionalIncident, error) {
	if err := s.authorize(ctx, userID, monitorID); err != nil {
		return nil, err
	}
	if !domain.ValidHubID(alertID) {
		return nil, domain.ErrValidation
	}
	incident, err := s.incidents.GetIncident(ctx, alertID)
	if err != nil {
		return nil, err
	}
	if incident.MonitorID != monitorID || incident.Scope != domain.IncidentScopeRegional || incident.ProbeID == domain.LocalProbeID {
		return nil, ports.ErrNotFound
	}
	return incident, nil
}

// Acknowledge queues an original-incident command using the authenticated actor.
func (s *ProbeAlertService) Acknowledge(ctx context.Context, userID, monitorID int64, alertID, commandID string, note *string) (*domain.ProbeCommand, error) {
	incident, err := s.incident(ctx, userID, monitorID, alertID)
	if err != nil {
		return nil, err
	}
	if incident.SubjectKind != domain.IncidentSubjectAvailability {
		return nil, domain.ErrValidation
	}
	if s.commands == nil || s.installation == nil {
		return nil, domain.ErrInternal
	}
	installation, err := s.installation.Get(ctx)
	if err != nil {
		return nil, err
	}
	return s.commands.IssueAcknowledgement(ctx, domain.ProbeAcknowledgementIssue{
		CommandID: commandID, HubID: installation.HubID, ProbeID: incident.ProbeID,
		SourceAlertID: incident.SourceAlertID, AssignmentGeneration: incident.AssignmentGeneration,
		RequestedBy: userID, ActorDisplayName: fmt.Sprintf("User %d", userID), Note: note, Lifetime: 24 * time.Hour,
	})
}

// Command returns a safe receipt only to its original requester or an admin who
// still has monitor visibility. A known UUID never widens access.
func (s *ProbeAlertService) Command(ctx context.Context, userID, monitorID int64, alertID, commandID string) (*domain.ProbeCommand, error) {
	incident, err := s.incident(ctx, userID, monitorID, alertID)
	if err != nil {
		return nil, err
	}
	if !domain.ValidHubID(commandID) {
		return nil, domain.ErrValidation
	}
	installation, err := s.installation.Get(ctx)
	if err != nil {
		return nil, err
	}
	command, err := s.receipts.GetCommand(ctx, installation.HubID, incident.ProbeID, commandID)
	if err != nil {
		return nil, err
	}
	if command.SourceAlertID == nil || *command.SourceAlertID != incident.SourceAlertID || command.Kind != "alert.ack" {
		return nil, ports.ErrNotFound
	}
	admin, err := s.access.IsAdmin(ctx, userID)
	if err != nil {
		return nil, err
	}
	if command.RequestedBy != userID && !admin {
		return nil, ports.ErrNotFound
	}
	return command, nil
}
