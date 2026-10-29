package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.LocalDeliveryConfigReader = (*ProbeActivationStore)(nil)

// ReadAppliedLocalDelivery retains work across unrelated configuration changes.
// It never executes old settings: the returned graph is the current authenticated
// graph, and final authorization must recheck it inside its own transaction.
func (r *ProbeActivationStore) ReadAppliedLocalDelivery(ctx context.Context, notificationID, revision int64) (*domain.LocalProbeConfigDefinition, error) {
	if r == nil || r.db == nil || r.encoder == nil || notificationID < 1 || revision < 1 {
		return nil, domain.ErrValidation
	}
	var result *domain.LocalProbeConfigDefinition
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		result = nil
		current, err := readAppliedLocalTx(ctx, tx, r.encoder)
		if err != nil {
			return err
		}
		match, err := localDeliveryChannelMatchesTx(ctx, tx, r.encoder, r.protector, current, notificationID, revision)
		if err == nil && match {
			result = current
		}
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ports.ErrConflict) || errors.Is(err, ports.ErrNotFound) {
			return nil, err
		}
		return nil, domain.ErrInternal // Decoder/storage diagnostics may contain secrets.
	}
	return result, nil
}

// localDeliveryChannelMatchesTx requires an applied origin and authenticates
// every applied revision through the current one. This also catches rotation or
// disable/restore ABA: matching endpoints alone must not revive old work.
// The caller holds the normal configuration-authority transaction.
func localDeliveryChannelMatchesTx(ctx context.Context, tx bun.Tx, encoder ports.LocalProbeConfigEncoder, protector ports.ProbeConfigProtector, current *domain.LocalProbeConfigDefinition, notificationID, revision int64) (bool, error) {
	if current == nil || revision < 1 || revision > current.Revision {
		return false, nil
	}
	if revision == current.Revision {
		return true, nil
	}
	comparer, ok := encoder.(ports.LocalProbeConfigChannelComparer)
	if !ok || protector == nil {
		return false, nil // No fallback to unauthenticated old credentials.
	}
	var receipts []probeConfigAppliedReceiptModel
	if err := tx.NewSelect().Model(&receipts).
		Where("probe_id = ? AND revision >= ? AND revision <= ?", domain.LocalProbeID, revision, current.Revision).
		OrderExpr("revision ASC").Scan(ctx); err != nil {
		return false, err
	}
	if len(receipts) == 0 || receipts[0].Revision != revision || receipts[len(receipts)-1].Revision != current.Revision {
		return false, nil
	}
	for _, receipt := range receipts {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		var snapshot probeConfigModel
		if err := tx.NewSelect().Model(&snapshot).
			Where("probe_id = ? AND revision = ?", domain.LocalProbeID, receipt.Revision).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		metadata := snapshot.domain().ProbeConfigMetadata
		if receipt.HubID != current.Target.HubID || snapshot.HubID != receipt.HubID || snapshot.SHA256 != receipt.SHA256 {
			return false, ports.ErrConflict
		}
		plain, err := protector.Open(ctx, metadata, snapshot.ProtectedPayload)
		if err != nil {
			return false, domain.ErrInternal
		}
		if fmt.Sprintf("%x", sha256.Sum256(plain)) != metadata.SHA256 {
			clear(plain)
			return false, domain.ErrInternal
		}
		match, err := comparer.SameLocalDeliveryChannel(plain, *current, notificationID)
		clear(plain)
		if err != nil {
			return false, domain.ErrInternal
		}
		if !match {
			return false, nil
		}
	}
	return true, nil
}
