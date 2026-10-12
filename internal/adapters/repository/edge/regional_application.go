package edge

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type regionalApplicationRow struct {
	bun.BaseModel      `bun:"table:edge_regional_delivery_applications"`
	ConfigRevision     int64  `bun:",pk"`
	MonitorID          int64  `bun:",pk"`
	StreamID           string `bun:",pk"`
	AssignmentRevision int64
	Generation         int64
	SHA256             string
	AlertDelivery      domain.AlertDelivery
}

func (r regionalApplicationRow) application(target domain.ProbeConfigTarget) domain.ProbeDeliveryModeApplication {
	return domain.ProbeDeliveryModeApplication{ProbeConfigTarget: target, MonitorID: r.MonitorID,
		AssignmentRevision: r.AssignmentRevision, Generation: r.Generation, ConfigRevision: r.ConfigRevision,
		SHA256: r.SHA256, StreamID: r.StreamID, AlertDelivery: r.AlertDelivery}
}

func writeRegionalApplications(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity, config domain.EdgeActiveConfig) error {
	members := make(map[int64]int64, len(config.Assignments))
	for _, assignment := range config.Assignments {
		members[assignment.MonitorID] = assignment.Generation
	}
	seen := make(map[int64]bool, len(config.RegionalDeliveryApplications))
	for _, fact := range config.RegionalDeliveryApplications {
		if !domain.ValidProbeDeliveryModeApplication(fact) || fact.AlertDelivery != domain.AlertDeliveryRegional ||
			fact.ProbeConfigTarget != config.Snapshot.ProbeConfigTarget || fact.ConfigRevision != config.Snapshot.Revision ||
			fact.SHA256 != config.Snapshot.SHA256 || fact.StreamID != identity.StreamID ||
			members[fact.MonitorID] != fact.Generation || seen[fact.MonitorID] {
			return domain.ErrValidation
		}
		seen[fact.MonitorID] = true
		row := regionalApplicationRow{ConfigRevision: fact.ConfigRevision, MonitorID: fact.MonitorID,
			StreamID: fact.StreamID, AssignmentRevision: fact.AssignmentRevision, Generation: fact.Generation,
			SHA256: fact.SHA256, AlertDelivery: domain.AlertDeliveryRegional}
		if _, err := tx.NewInsert().Model(&row).On("CONFLICT (config_revision, monitor_id, stream_id) DO NOTHING").Exec(ctx); err != nil {
			return err
		}
		var stored regionalApplicationRow
		if err := tx.NewSelect().Model(&stored).Where("config_revision = ? AND monitor_id = ? AND stream_id = ?", fact.ConfigRevision, fact.MonitorID, fact.StreamID).Scan(ctx); err != nil {
			return err
		}
		if stored.application(fact.ProbeConfigTarget) != fact {
			return ports.ErrConflict
		}
	}
	return nil
}

func readRegionalApplications(ctx context.Context, db bun.IDB, metadata domain.ProbeConfigMetadata, streamID string) ([]domain.ProbeDeliveryModeApplication, error) {
	var rows []regionalApplicationRow
	if err := db.NewSelect().Model(&rows).Where("config_revision = ? AND stream_id = ?", metadata.Revision, streamID).Order("monitor_id ASC").Scan(ctx); err != nil {
		return nil, err
	}
	var facts []domain.ProbeDeliveryModeApplication
	for _, row := range rows {
		fact := row.application(metadata.ProbeConfigTarget)
		if !domain.ValidProbeDeliveryModeApplication(fact) || fact.AlertDelivery != domain.AlertDeliveryRegional || fact.SHA256 != metadata.SHA256 {
			return nil, ports.ErrConflict
		}
		facts = append(facts, fact)
	}
	return facts, nil
}

// requireRegionalApplication validates present effective-mode state in the same
// write transaction as availability recording or final provider authorization.
// Absence retains the legacy regional runtime but supplies no application fact.
// This deliberately implements no aggregate/both write or suppression branch.
func requireRegionalApplication(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity, monitorID, generation, revision int64) error {
	var row regionalApplicationRow
	if err := tx.NewSelect().Model(&row).Where("config_revision = ? AND monitor_id = ? AND stream_id = ?", revision, monitorID, identity.StreamID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	var config configRow
	if err := tx.NewSelect().Model(&config).Where("revision = ?", revision).Scan(ctx); err != nil {
		return err
	}
	fact := row.application(domain.ProbeConfigTarget{HubID: identity.HubID, ProbeID: identity.ProbeID})
	if revision != identity.ConfigRevision || config.HubID != identity.HubID || config.ProbeID != identity.ProbeID ||
		!domain.ValidProbeDeliveryModeApplication(fact) || fact.Generation != generation || fact.SHA256 != config.SHA256 || fact.AlertDelivery != domain.AlertDeliveryRegional {
		return ports.ErrConflict
	}
	return nil
}
