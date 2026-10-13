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

type remoteRegionalApplicationModel struct {
	bun.BaseModel        `bun:"table:probe_remote_regional_applications"`
	ProbeID              string `bun:",pk"`
	ConfigRevision       int64  `bun:",pk"`
	MonitorID            int64  `bun:",pk"`
	StreamID             string `bun:",pk"`
	AssignmentRevision   int64
	Generation           int64
	HubID                string
	SHA256               string
	AlertDelivery        domain.AlertDelivery
	ConnectionGeneration int64
	RecordedAt           time.Time
}

var _ ports.RemoteRegionalApplicationRepository = (*RemoteProbeConfigSyncStore)(nil)

// RecordRemoteRegionalApplications stores authenticated effective regional facts
// for one exact committed document. Queue order, desired rows and configuration
// ACKs never substitute for the stored session/receipt/snapshot checks.
func (s *RemoteProbeConfigSyncStore) RecordRemoteRegionalApplications(ctx context.Context, session domain.ProbeReplaySession, applications []domain.ProbeDeliveryModeApplication) error {
	if s == nil || s.db == nil || s.decoder == nil || s.protector == nil || len(applications) == 0 || len(applications) > 256 ||
		!domain.ValidHubID(session.HubID) || !validRemoteProbeID(session.ProbeID) ||
		!domain.ValidHubID(session.OwnerID) || !domain.ValidHubID(session.StreamID) ||
		session.StreamID == domain.LocalStreamID || session.ConnectionGeneration <= 0 {
		return domain.ErrValidation
	}
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		authority, err := lockProbeSession(ctx, tx, session, s.protector.KeyHash(session.HubID))
		if err != nil {
			return err
		}
		target := domain.ProbeConfigTarget{HubID: session.HubID, ProbeID: session.ProbeID}
		document := applications[0]
		if document.ProbeConfigTarget != target || document.StreamID != session.StreamID || document.ConfigRevision <= 0 || !domain.ValidKeyHash(document.SHA256) {
			return domain.ErrValidation
		}
		var active probeActiveConfigModel
		if err := tx.NewSelect().Model(&active).Where("probe_id = ?", session.ProbeID).Scan(ctx); err != nil {
			return err
		}
		var receipt probeConfigAppliedReceiptModel
		if err := tx.NewSelect().Model(&receipt).Where("probe_id = ? AND revision = ?", session.ProbeID, document.ConfigRevision).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ports.ErrConflict // Queue order does not prove a committed receipt.
			}
			return err
		}
		var snapshot probeConfigModel
		if err := tx.NewSelect().Model(&snapshot).Where("probe_id = ? AND revision = ?", session.ProbeID, document.ConfigRevision).Scan(ctx); err != nil {
			return err
		}
		if !domain.ValidProbeActiveConfig(domain.ProbeActiveConfig{ProbeConfigTarget: target, Revision: active.Revision, SHA256: active.SHA256, AppliedAt: active.AppliedAt, AssignmentCount: active.AssignmentCount}) || active.HubID != session.HubID || active.Revision != document.ConfigRevision || active.SHA256 != document.SHA256 || receipt.HubID != active.HubID || receipt.SHA256 != active.SHA256 || receipt.AssignmentCount != active.AssignmentCount || !receipt.AppliedAt.Equal(active.AppliedAt) || snapshot.HubID != active.HubID || snapshot.SHA256 != active.SHA256 {
			return ports.ErrConflict
		}
		plain, err := s.protector.Open(ctx, snapshot.domain().ProbeConfigMetadata, snapshot.ProtectedPayload)
		if err != nil {
			return err
		}
		defer clear(plain)
		resolved, err := s.decoder.DecodeEdge(ctx, plain, target)
		if err != nil || resolved == nil || !domain.SameProbeConfigMetadata(resolved.Metadata, snapshot.domain().ProbeConfigMetadata) || len(resolved.Assignments) != active.AssignmentCount {
			return ports.ErrConflict
		}
		members := make(map[int64]domain.EdgeResolvedAssignment, len(resolved.Assignments))
		for _, assignment := range resolved.Assignments {
			if assignment.Monitor == nil {
				return ports.ErrConflict
			}
			if _, duplicate := members[assignment.Monitor.ID]; duplicate {
				return ports.ErrConflict
			}
			members[assignment.Monitor.ID] = assignment
		}
		seen := make(map[int64]bool, len(applications))
		for _, fact := range applications {
			member, exists := members[fact.MonitorID]
			if seen[fact.MonitorID] || !exists || !domain.ValidProbeDeliveryModeApplication(fact) || fact.ProbeConfigTarget != target || fact.StreamID != session.StreamID || fact.ConfigRevision != document.ConfigRevision || fact.SHA256 != document.SHA256 || fact.AlertDelivery != domain.AlertDeliveryRegional || fact.Generation != member.Generation || fact.AssignmentRevision != member.AssignmentRevision {
				return ports.ErrConflict
			}
			seen[fact.MonitorID] = true
			row := remoteRegionalApplicationModel{ProbeID: fact.ProbeID, ConfigRevision: fact.ConfigRevision, MonitorID: fact.MonitorID, StreamID: fact.StreamID, AssignmentRevision: fact.AssignmentRevision, Generation: fact.Generation, HubID: fact.HubID, SHA256: fact.SHA256, AlertDelivery: domain.AlertDeliveryRegional, ConnectionGeneration: session.ConnectionGeneration, RecordedAt: authority.now.UTC().Truncate(time.Microsecond)}
			var stored remoteRegionalApplicationModel
			err := tx.NewSelect().Model(&stored).Where("probe_id = ? AND config_revision = ? AND monitor_id = ? AND stream_id = ?", row.ProbeID, row.ConfigRevision, row.MonitorID, row.StreamID).Scan(ctx)
			if err == nil {
				if stored.AssignmentRevision != row.AssignmentRevision || stored.Generation != row.Generation || stored.HubID != row.HubID || stored.SHA256 != row.SHA256 || stored.AlertDelivery != row.AlertDelivery || stored.ConnectionGeneration <= 0 || stored.RecordedAt.IsZero() {
					return ports.ErrConflict
				}
				continue // First provenance/time remains immutable on reconnect.
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
				return err
			}
		}
		current, err := readProbeSession(ctx, tx, session.ProbeID)
		if err != nil {
			return err
		}
		finalNow, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if current.OwnerID != session.OwnerID || current.Generation != session.ConnectionGeneration || current.LeaseUntil <= finalNow.Unix() {
			return ports.ErrConflict
		}
		return nil
	})
	return remoteSyncError(ctx, err)
}
