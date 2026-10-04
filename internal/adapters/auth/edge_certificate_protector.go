package auth

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.EdgeCertificateProtector = (*ProbeConfigProtector)(nil)

// SealCertificate protects local PEM with a dedicated purpose and full scope.
func (p *ProbeConfigProtector) SealCertificate(ctx context.Context, m domain.EdgeCertificateMetadata, pem []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil || p.aead == nil || !domain.ValidEdgeCertificateMetadata(m) || len(pem) == 0 || len(pem) > domain.MaxEdgeCertificateBytes {
		return nil, domain.ErrValidation
	}
	return p.aead.Seal([]byte{1}, nil, pem, certificateAssociatedData(m)), nil
}

// OpenCertificate authenticates ciphertext before returning confidential PEM.
func (p *ProbeConfigProtector) OpenCertificate(ctx context.Context, m domain.EdgeCertificateMetadata, data []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil || p.aead == nil || !domain.ValidEdgeCertificateMetadata(m) || len(data) <= domain.ProbeConfigProtectionOverhead || len(data) > domain.MaxEdgeCertificateBytes+domain.ProbeConfigProtectionOverhead || data[0] != 1 {
		return nil, domain.ErrValidation
	}
	pem, err := p.aead.Open(nil, nil, data[1:], certificateAssociatedData(m))
	if err != nil {
		return nil, errors.New("local certificate authentication failed")
	}
	return pem, nil
}

func certificateAssociatedData(m domain.EdgeCertificateMetadata) []byte {
	data, _ := json.Marshal([]any{"phoenix.edge.certificate.aes256gcm.v1", m.HubID, m.ProbeID, m.StreamID, m.RotationID, m.Version, m.Fingerprint, m.CreatedAt.UTC().Format(time.RFC3339Nano), m.NotBefore.UTC().Format(time.RFC3339), m.NotAfter.UTC().Format(time.RFC3339)})
	return data
}
