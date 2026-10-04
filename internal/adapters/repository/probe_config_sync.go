package repository

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// RemoteProbeConfigSyncStore reuses protected snapshots and application receipts
// as durable sync work. Publication and source capture share one transaction.
type RemoteProbeConfigSyncStore struct {
	db        *bun.DB
	encoder   ports.RemoteProbeConfigEncoder
	decoder   ports.EdgeConfigDecoder
	protector ports.ProbeConfigProtector
}

var _ ports.RemoteProbeConfigSyncRepository = (*RemoteProbeConfigSyncStore)(nil)

// NewRemoteProbeConfigSyncStore requires the installed edge semantic validator
// and the installation's verified protector. No network I/O occurs in this store.
func NewRemoteProbeConfigSyncStore(db *bun.DB, encoder ports.RemoteProbeConfigEncoder, decoder ports.EdgeConfigDecoder, protector ports.ProbeConfigProtector) *RemoteProbeConfigSyncStore {
	return &RemoteProbeConfigSyncStore{db: db, encoder: encoder, decoder: decoder, protector: protector}
}

// RefreshRemote publishes a revision only when the saved complete graph changes.
// Persisted source repairs missed wakeups; retained snapshots survive lost sends.
func (s *RemoteProbeConfigSyncStore) RefreshRemote(ctx context.Context, target domain.ProbeConfigTarget, at time.Time) (domain.ProbeConfigMetadata, error) {
	var result domain.ProbeConfigMetadata
	if s == nil || s.db == nil || s.encoder == nil || s.decoder == nil || s.protector == nil || !domain.ValidProbeConfigTarget(target) || !validRemoteProbeID(target.ProbeID) || at.IsZero() {
		return result, domain.ErrValidation
	}
	at = at.UTC().Truncate(time.Microsecond)
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		// Match registry/source lock order. SERIALIZABLE also protects relationship
		// predicates against inserts/deletes while the complete document is built.
		if _, err := tx.ExecContext(ctx, "UPDATE probes SET id = id WHERE id = ?", target.ProbeID); err != nil {
			return err
		}
		var installation probeInstallationModel
		if err := tx.NewSelect().Model(&installation).Where("id = 1").Scan(ctx); err != nil {
			return err
		}
		if installation.HubID != target.HubID || installation.KeyHash != s.protector.KeyHash(target.HubID) {
			return domain.ErrProbeKeyMismatch
		}
		source, err := readProbeConfigSource(ctx, tx, target.ProbeID)
		if err != nil {
			return err
		}
		definition, err := services.ResolveRemoteProbeConfig(source)
		if err != nil {
			return err
		}
		definition.Target = target
		var latest probeConfigModel
		err = tx.NewSelect().Model(&latest).Where("probe_id = ?", target.ProbeID).Order("revision DESC").Limit(1).Scan(ctx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if latest.HubID != target.HubID {
				return ports.ErrConflict
			}
			// Corruption must never be hidden by replacing a retained document.
			plain, err := s.protector.Open(ctx, latest.domain().ProbeConfigMetadata, latest.ProtectedPayload)
			if err != nil {
				return err
			}
			clear(plain)
			definition.Revision = latest.Revision
			definition.CreatedAt, definition.EffectiveAt = latest.CreatedAt.UTC(), latest.EffectiveAt.UTC()
			document, metadata, err := s.encode(ctx, definition)
			clear(document)
			if err != nil {
				return err
			}
			if metadata.SHA256 == latest.SHA256 {
				result = latest.domain().ProbeConfigMetadata
				return nil
			}
		}
		if latest.Revision == math.MaxInt64 {
			return ports.ErrConflict
		}
		definition.Revision, definition.CreatedAt, definition.EffectiveAt = latest.Revision+1, at, at
		document, metadata, err := s.encode(ctx, definition)
		if err != nil {
			return err
		}
		defer clear(document)
		protected, err := s.protector.Seal(ctx, metadata, document)
		if err != nil {
			return err
		}
		row := probeConfigModel{ProbeID: target.ProbeID, HubID: target.HubID, Revision: metadata.Revision, SchemaVersion: metadata.SchemaVersion, SHA256: metadata.SHA256,
			CreatedAt: metadata.CreatedAt, EffectiveAt: metadata.EffectiveAt, ProtectedPayload: protected, StoredAt: at}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		result = metadata
		return nil
	})
	if err != nil {
		return domain.ProbeConfigMetadata{}, remoteSyncError(ctx, err)
	}
	return result, nil
}

func (s *RemoteProbeConfigSyncStore) encode(ctx context.Context, definition domain.LocalProbeConfigDefinition) ([]byte, domain.ProbeConfigMetadata, error) {
	document, err := s.encoder.EncodeRemote(definition)
	if err != nil {
		return nil, domain.ProbeConfigMetadata{}, err
	}
	resolved, err := s.decoder.DecodeEdge(ctx, document, definition.Target)
	if err != nil {
		clear(document)
		return nil, domain.ProbeConfigMetadata{}, err
	}
	return document, resolved.Metadata, nil
}

// RecordRemoteApplied records an exact retained snapshot after validated wire
// acknowledgement. Lease checks and both durable receipt writes are atomic.
func (s *RemoteProbeConfigSyncStore) RecordRemoteApplied(ctx context.Context, lease domain.ProbeConnectorLease, receipt domain.ProbeActiveConfig) error {
	if s == nil || s.db == nil || s.protector == nil || s.decoder == nil || !validRemoteProbeID(lease.ProbeID) || receipt.ProbeID != lease.ProbeID || !domain.ValidProbeConfigTarget(receipt.ProbeConfigTarget) || receipt.Revision <= 0 || !domain.ValidKeyHash(receipt.SHA256) || receipt.AssignmentCount < 0 || receipt.AppliedAt.IsZero() {
		return domain.ErrValidation
	}
	err := NewProbeConnectorStore(s.db).transaction(ctx, lease.ProbeID, func(ctx context.Context, tx bun.Tx, enabled bool, now int64) error {
		session, err := readProbeSession(ctx, tx, lease.ProbeID)
		if err != nil {
			return err
		}
		if !enabled || !matchesProbeSession(session, lease) || session.LeaseUntil <= now {
			return ports.ErrConflict
		}
		var snapshot probeConfigModel
		if err := tx.NewSelect().Model(&snapshot).Where("probe_id = ? AND revision = ?", receipt.ProbeID, receipt.Revision).Scan(ctx); err != nil {
			return err
		}
		if snapshot.HubID != receipt.HubID || snapshot.SHA256 != receipt.SHA256 {
			return ports.ErrConflict
		}
		plain, err := s.protector.Open(ctx, snapshot.domain().ProbeConfigMetadata, snapshot.ProtectedPayload)
		if err != nil {
			return err
		}
		defer clear(plain)
		resolved, err := s.decoder.DecodeEdge(ctx, plain, receipt.ProbeConfigTarget)
		if err != nil {
			return err
		}
		if len(resolved.Assignments) != receipt.AssignmentCount {
			return ports.ErrConflict
		}
		var active probeActiveConfigModel
		err = tx.NewSelect().Model(&active).Where("probe_id = ?", receipt.ProbeID).Scan(ctx)
		missing := errors.Is(err, sql.ErrNoRows)
		if err != nil && !missing {
			return err
		}
		if !missing && active.Revision > receipt.Revision {
			return ports.ErrConflict
		}
		var prior probeConfigAppliedReceiptModel
		err = tx.NewSelect().Model(&prior).Where("probe_id = ? AND revision = ?", receipt.ProbeID, receipt.Revision).Scan(ctx)
		if err == nil {
			if prior.HubID != receipt.HubID || prior.SHA256 != receipt.SHA256 || prior.AssignmentCount != receipt.AssignmentCount || missing || active.Revision != receipt.Revision || active.SHA256 != receipt.SHA256 {
				return ports.ErrConflict
			}
			return nil // First committed time is immutable on a repeated receipt.
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		active = probeActiveConfigModel{ProbeID: receipt.ProbeID, HubID: receipt.HubID, Revision: receipt.Revision, SHA256: receipt.SHA256, AppliedAt: receipt.AppliedAt.UTC().Truncate(time.Microsecond), AssignmentCount: receipt.AssignmentCount}
		if missing {
			_, err = tx.NewInsert().Model(&active).Exec(ctx)
		} else {
			_, err = tx.NewUpdate().Model(&active).WherePK().Exec(ctx)
		}
		if err != nil {
			return err
		}
		prior = probeConfigAppliedReceiptModel{ProbeID: active.ProbeID, HubID: active.HubID, Revision: active.Revision, SHA256: active.SHA256, AppliedAt: active.AppliedAt, AssignmentCount: active.AssignmentCount, CreatedAt: time.Unix(now, 0).UTC()}
		_, err = tx.NewInsert().Model(&prior).Exec(ctx)
		return err
	})
	return remoteSyncError(ctx, err)
}

func remoteSyncError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, sentinel := range []error{ports.ErrConflict, ports.ErrNotFound, domain.ErrValidation, domain.ErrProbeKeyMismatch} {
		if errors.Is(err, sentinel) {
			return sentinel
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ports.ErrNotFound
	}
	return domain.ErrInternal // SQL/extension errors may contain confidential data.
}
