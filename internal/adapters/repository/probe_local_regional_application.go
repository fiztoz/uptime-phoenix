package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type localRegionalApplicationModel struct {
	bun.BaseModel      `bun:"table:probe_local_regional_applications"`
	ProbeID            string `bun:",pk"`
	ConfigRevision     int64  `bun:",pk"`
	MonitorID          int64  `bun:",pk"`
	AssignmentRevision int64
	Generation         int64
	HubID              string
	StreamID           string
	SHA256             string
	AlertDelivery      domain.AlertDelivery
}

func (r localRegionalApplicationModel) application() domain.ProbeDeliveryModeApplication {
	return domain.ProbeDeliveryModeApplication{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: r.HubID, ProbeID: r.ProbeID},
		MonitorID: r.MonitorID, AssignmentRevision: r.AssignmentRevision, Generation: r.Generation,
		ConfigRevision: r.ConfigRevision, SHA256: r.SHA256, StreamID: r.StreamID, AlertDelivery: r.AlertDelivery}
}

// writeLocalRegionalApplicationsTx records only the mode this source implements
// after the entire source graph has matched the candidate's exact hash. It is
// never called on a receipt retry or from a desired-mode/document ACK projection.
func writeLocalRegionalApplicationsTx(ctx context.Context, tx bun.Tx, definition domain.LocalProbeConfigDefinition, hash string) error {
	for _, assignment := range definition.Assignments {
		if assignment.AssignmentRevision == 0 {
			continue
		}
		if assignment.Monitor == nil {
			return domain.ErrValidation
		}
		row := localRegionalApplicationModel{ProbeID: domain.LocalProbeID, ConfigRevision: definition.Revision,
			MonitorID: assignment.Monitor.ID, AssignmentRevision: assignment.AssignmentRevision, Generation: assignment.Generation,
			HubID: definition.Target.HubID, StreamID: domain.LocalStreamID, SHA256: hash, AlertDelivery: domain.AlertDeliveryRegional}
		if definition.Target.ProbeID != domain.LocalProbeID || !domain.ValidProbeDeliveryModeApplication(row.application()) {
			return domain.ErrValidation
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// readLocalRegionalApplicationsTx validates present source-local state against
// the exact execution graph. Missing/partial pre-upgrade state is unreported;
// the refresher publishes a new complete revision without mutating old facts.
func readLocalRegionalApplicationsTx(ctx context.Context, tx bun.Tx, definition *domain.LocalProbeConfigDefinition, hash string) error {
	var rows []localRegionalApplicationModel
	if err := tx.NewSelect().Model(&rows).Where("probe_id = ? AND config_revision = ?", domain.LocalProbeID, definition.Revision).Order("monitor_id ASC").Scan(ctx); err != nil {
		return err
	}
	members := make(map[int64]domain.ProbeConfigAssignment, len(definition.Assignments))
	for _, member := range definition.Assignments {
		if member.Monitor == nil {
			return ports.ErrConflict
		}
		members[member.Monitor.ID] = member
	}
	var facts []domain.ProbeDeliveryModeApplication
	for _, row := range rows {
		fact := row.application()
		member, exists := members[fact.MonitorID]
		if !exists || !domain.ValidProbeDeliveryModeApplication(fact) || fact.ProbeConfigTarget != definition.Target ||
			fact.SHA256 != hash || fact.StreamID != domain.LocalStreamID || fact.AlertDelivery != domain.AlertDeliveryRegional ||
			fact.Generation != member.Generation || fact.AssignmentRevision != member.AssignmentRevision {
			return ports.ErrConflict
		}
		facts = append(facts, fact)
	}
	definition.RegionalDeliveryApplications = facts
	return nil
}

// requireLocalRegionalApplicationTx adds present-state consistency checks to a
// protected local heartbeat transaction. Existing lease/member/pointer fences
// remain authoritative. Absence supplies no mode authority and never suppresses.
func requireLocalRegionalApplicationTx(ctx context.Context, tx bun.Tx, active probeActiveConfigModel, monitorID, generation, assignmentRevision int64, streamID string) error {
	var row localRegionalApplicationModel
	if err := tx.NewSelect().Model(&row).Where("probe_id = ? AND config_revision = ? AND monitor_id = ?", domain.LocalProbeID, active.Revision, monitorID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	fact := row.application()
	if !domain.ValidProbeDeliveryModeApplication(fact) || fact.HubID != active.HubID || fact.SHA256 != active.SHA256 ||
		fact.Generation != generation || fact.AssignmentRevision != assignmentRevision ||
		fact.StreamID != domain.LocalStreamID || fact.StreamID != streamID || fact.AlertDelivery != domain.AlertDeliveryRegional {
		return ports.ErrConflict
	}
	return nil
}
