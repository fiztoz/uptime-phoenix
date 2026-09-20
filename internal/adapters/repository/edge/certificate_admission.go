package edge

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Invalid eligibility returns false without an error so the caller can commit
// an irreversible expiry observation while refusing the session generation.
func certificateAdmission(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity, binding domain.EdgeEnrollment, now time.Time) (*time.Time, bool, error) {
	if err := expireCertificateOverlaps(ctx, tx, now); err != nil {
		return nil, false, err
	}
	state, err := readCertificateState(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	if binding.CertificateFingerprint == "" {
		// Legacy callers are valid only before certificate rotation exists. The
		// configured TLS runtime additionally requires a handshake binding even
		// in bootstrap state; this keeps older narrow repository clients usable.
		return nil, state.HighestVersion == 1 && binding.CertificateNotBefore.IsZero() && binding.CertificateNotAfter.IsZero(), nil
	}
	if !domain.ValidKeyHash(binding.CertificateFingerprint) || binding.CertificateNotBefore.IsZero() || !binding.CertificateNotAfter.After(binding.CertificateNotBefore) || now.Before(binding.CertificateNotBefore) || !now.Before(binding.CertificateNotAfter) {
		return nil, false, nil
	}
	current, err := certificateMetadataForVersion(ctx, tx, identity, state.ActiveVersion)
	if err != nil {
		return nil, false, err
	}
	deadline := binding.CertificateNotAfter.UTC()
	var overlap edgeCertificateRotation
	err = tx.NewSelect().Model(&overlap).Where("version = ?", state.HighestVersion).Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	open := err == nil && !overlap.OverlapClosed && now.UnixMicro() >= overlap.PreparedAt && now.UnixMicro() < overlap.OverlapExpiresAt
	selected := current
	if binding.CertificateFingerprint != current.Fingerprint {
		if !open || overlap.ActivatedAt == nil || overlap.Version != state.ActiveVersion {
			return nil, false, nil
		}
		selected, err = certificateMetadataForVersion(ctx, tx, identity, overlap.PreviousVersion)
		if err != nil {
			return nil, false, err
		}
		limit := time.UnixMicro(overlap.OverlapExpiresAt).UTC()
		if limit.Before(deadline) {
			deadline = limit
		}
	} else if open && overlap.ActivatedAt == nil && overlap.PreviousVersion == state.ActiveVersion {
		limit := time.UnixMicro(overlap.OverlapExpiresAt).UTC()
		if limit.Before(deadline) {
			deadline = limit
		}
	}
	if selected.Fingerprint != binding.CertificateFingerprint {
		return nil, false, nil
	}
	if selected.Version > 1 && (!selected.NotBefore.Equal(binding.CertificateNotBefore) || !selected.NotAfter.Equal(binding.CertificateNotAfter)) {
		return nil, false, nil
	}
	return &deadline, true, nil
}

func certificateMetadataForVersion(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity, version int64) (domain.EdgeCertificateMetadata, error) {
	if version == 1 {
		return domain.EdgeCertificateMetadata{Version: 1, Fingerprint: identity.Fingerprint}, nil
	}
	var row edgeCertificateRotation
	if err := tx.NewSelect().Model(&row).Where("version = ?", version).Scan(ctx); err != nil {
		return domain.EdgeCertificateMetadata{}, ErrStorage
	}
	m := row.protected().EdgeCertificateMetadata
	if row.ActivatedAt == nil || !domain.ValidEdgeCertificateMetadata(m) || m.HubID != identity.HubID || m.ProbeID != identity.ProbeID || m.StreamID != identity.StreamID {
		return domain.EdgeCertificateMetadata{}, ErrStorage
	}
	return m, nil
}
