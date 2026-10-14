package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type localDeliveryPreparationModel struct {
	bun.BaseModel      `bun:"table:probe_local_delivery_preparations"`
	ProbeID            string `bun:",pk"`
	ConfigRevision     int64  `bun:",pk"`
	MonitorID          int64  `bun:",pk"`
	AssignmentRevision int64
	Generation         int64
	HubID              string
	StreamID           string
	SHA256             string
	AlertDelivery      domain.AlertDelivery
	PreparedAt         time.Time
}

func (r localDeliveryPreparationModel) preparation() domain.LocalDeliveryPreparation {
	return domain.LocalDeliveryPreparation{ProbeDeliveryModeApplication: domain.ProbeDeliveryModeApplication{
		ProbeConfigTarget: domain.ProbeConfigTarget{HubID: r.HubID, ProbeID: r.ProbeID}, MonitorID: r.MonitorID,
		AssignmentRevision: r.AssignmentRevision, Generation: r.Generation, ConfigRevision: r.ConfigRevision,
		SHA256: r.SHA256, StreamID: r.StreamID, AlertDelivery: r.AlertDelivery}, PreparedAt: r.PreparedAt.UTC()}
}

// writeLocalDeliveryPreparationsTx runs only on fresh exact whole-graph activation,
// after honest regional facts. It never executes on an immutable receipt retry.
func writeLocalDeliveryPreparationsTx(ctx context.Context, tx bun.Tx, definition domain.LocalProbeConfigDefinition, hash string) error {
	if err := readLocalRegionalApplicationsTx(ctx, tx, &definition, hash); err != nil {
		return err
	}
	regional := make(map[int64]domain.ProbeDeliveryModeApplication, len(definition.RegionalDeliveryApplications))
	for _, fact := range definition.RegionalDeliveryApplications {
		regional[fact.MonitorID] = fact
	}
	var now time.Time
	for _, member := range definition.Assignments {
		if member.AlertDelivery != domain.AlertDeliveryAggregate && member.AlertDelivery != domain.AlertDeliveryBoth {
			continue
		}
		if member.Monitor == nil || member.AssignmentRevision <= 0 {
			return ports.ErrConflict
		}
		fact, ok := regional[member.Monitor.ID]
		if !ok {
			return ports.ErrConflict
		}
		if now.IsZero() {
			var err error
			now, err = replayDatabaseTime(ctx, tx)
			if err != nil {
				return err
			}
			now = now.UTC().Truncate(time.Microsecond)
		}
		row := localDeliveryPreparationModel{ProbeID: fact.ProbeID, ConfigRevision: fact.ConfigRevision, MonitorID: fact.MonitorID,
			AssignmentRevision: fact.AssignmentRevision, Generation: fact.Generation, HubID: fact.HubID, StreamID: fact.StreamID,
			SHA256: fact.SHA256, AlertDelivery: member.AlertDelivery, PreparedAt: now}
		if !domain.ValidLocalDeliveryPreparation(row.preparation()) {
			return ports.ErrConflict
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

var _ ports.LocalDeliveryPreparationReader = (*ProbeActivationStore)(nil)

// ReadLocalDeliveryPreparations validates one selected protected local execution
// graph, receipt and regional facts, returning only request tuples. It is separate
// from execution reads: preparation rows never add heartbeat or send authority.
func (r *ProbeActivationStore) ReadLocalDeliveryPreparations(ctx context.Context, want domain.ProbeActiveConfig) ([]domain.LocalDeliveryPreparation, error) {
	if r == nil || r.db == nil || r.encoder == nil || want.ProbeID != domain.LocalProbeID || !domain.ValidProbeActiveConfig(want) {
		return nil, domain.ErrValidation
	}
	var preparations []domain.LocalDeliveryPreparation
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		definition, err := readAppliedLocalTx(ctx, tx, r.encoder)
		if err != nil {
			return err
		}
		var installation probeInstallationModel
		if err := tx.NewSelect().Model(&installation).Where("id = 1").Scan(ctx); err != nil {
			return err
		}
		if installation.HubID != want.HubID {
			return ports.ErrConflict
		}
		var active probeActiveConfigModel
		if err := tx.NewSelect().Model(&active).Where("probe_id = ?", domain.LocalProbeID).Scan(ctx); err != nil {
			return err
		}
		if active.HubID != want.HubID || active.Revision != want.Revision || active.SHA256 != want.SHA256 || !active.AppliedAt.Equal(want.AppliedAt) || active.AssignmentCount != want.AssignmentCount || len(definition.Assignments) != want.AssignmentCount {
			return ports.ErrConflict
		}
		var receipt probeConfigAppliedReceiptModel
		if err := tx.NewSelect().Model(&receipt).Where("probe_id = ? AND revision = ?", domain.LocalProbeID, want.Revision).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ports.ErrConflict
			}
			return err
		}
		if receipt.HubID != active.HubID || receipt.SHA256 != active.SHA256 || receipt.AssignmentCount != active.AssignmentCount || !receipt.AppliedAt.Equal(active.AppliedAt) {
			return ports.ErrConflict
		}
		regional := make(map[int64]domain.ProbeDeliveryModeApplication, len(definition.RegionalDeliveryApplications))
		for _, fact := range definition.RegionalDeliveryApplications {
			regional[fact.MonitorID] = fact
		}
		members := make(map[int64]domain.ProbeConfigAssignment, len(definition.Assignments))
		for _, member := range definition.Assignments {
			if member.Monitor == nil {
				return ports.ErrConflict
			}
			members[member.Monitor.ID] = member
		}
		var rows []localDeliveryPreparationModel
		if err := tx.NewSelect().Model(&rows).Where("probe_id = ? AND config_revision = ?", domain.LocalProbeID, want.Revision).Order("monitor_id ASC").Scan(ctx); err != nil {
			return err
		}
		// Retry callbacks must not append the previous transaction's provisional output.
		selected := make([]domain.LocalDeliveryPreparation, 0, len(rows))
		for _, row := range rows {
			p := row.preparation()
			member, exists := members[p.MonitorID]
			fact, reported := regional[p.MonitorID]
			if !exists || !reported || !domain.ValidLocalDeliveryPreparation(p) || p.ProbeConfigTarget != want.ProbeConfigTarget || p.ConfigRevision != want.Revision || p.SHA256 != want.SHA256 ||
				p.AssignmentRevision != member.AssignmentRevision || p.Generation != member.Generation || p.AlertDelivery != member.AlertDelivery {
				return ports.ErrConflict
			}
			bound := p.ProbeDeliveryModeApplication
			bound.AlertDelivery = domain.AlertDeliveryRegional
			if bound != fact {
				return ports.ErrConflict
			}
			selected = append(selected, p)
		}
		preparations = selected
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ports.ErrNotFound) {
			return nil, ports.ErrNotFound
		}
		if errors.Is(err, ports.ErrConflict) {
			return nil, ports.ErrConflict
		}
		return nil, domain.ErrInternal // SQL/encoder failures may contain confidential data.
	}
	return preparations, nil
}
