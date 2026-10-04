package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// CertificateCommandCodec maps existing closed DTOs to nonsecret certificate input.
type CertificateCommandCodec struct{}

var _ ports.ProbeCertificateCommandCodec = CertificateCommandCodec{}

// EncodeCertificateCommand returns an exact bounded request with no private key.
func (codec CertificateCommandCodec) EncodeCertificateCommand(ctx context.Context, c domain.ProbeCertificateCommand) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request := CommandRequest{CommandID: c.CommandID, Kind: c.Kind, CreatedAt: Timestamp(c.CreatedAt.UTC()), ExpiresAt: Timestamp(c.ExpiresAt.UTC()), Target: CommandTarget{ProbeID: c.ProbeID}}
	switch c.Kind {
	case CommandCertificatePrepare:
		if c.ExpectedFingerprint != "" {
			return nil, domain.ErrValidation
		}
		request.Data = CertificatePrepareData{RotationID: c.RotationID, CertificateVersion: Decimal(c.CertificateVersion), ValidForDays: int64(c.ValidForDays)}
	case CommandCertificateActivate:
		if c.ValidForDays != 0 {
			return nil, domain.ErrValidation
		}
		request.Data = CertificateActivateData{RotationID: c.RotationID, CertificateVersion: Decimal(c.CertificateVersion), ExpectedFingerprint: c.ExpectedFingerprint}
	default:
		return nil, domain.ErrValidation
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, domain.ErrValidation
	}
	if _, err := codec.DecodeCertificateCommand(ctx, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// DecodeCertificateCommand binds the digest to original wire bytes, preserving
// insignificant JSON whitespace that is significant to command retry identity.
func (CertificateCommandCodec) DecodeCertificateCommand(ctx context.Context, payload []byte) (domain.ProbeCertificateCommand, error) {
	if err := ctx.Err(); err != nil {
		return domain.ProbeCertificateCommand{}, err
	}
	if len(payload) == 0 || len(payload) > domain.MaxProbeCommandBytes || !bytes.Equal(payload, bytes.TrimSpace(payload)) {
		return domain.ProbeCertificateCommand{}, domain.ErrValidation
	}
	fields, err := decodeJSONObject(payload)
	if err != nil {
		return domain.ProbeCertificateCommand{}, domain.ErrValidation
	}
	request, err := decodeCommandRequestFields(fields)
	if err != nil {
		return domain.ProbeCertificateCommand{}, domain.ErrValidation
	}
	c := domain.ProbeCertificateCommand{CommandID: request.CommandID, ProbeID: request.Target.ProbeID, Kind: request.Kind, CreatedAt: time.Time(request.CreatedAt).UTC(), ExpiresAt: time.Time(request.ExpiresAt).UTC(), PayloadHash: sha256.Sum256(payload)}
	switch data := request.Data.(type) {
	case CertificatePrepareData:
		c.RotationID, c.CertificateVersion, c.ValidForDays = data.RotationID, int64(data.CertificateVersion), int(data.ValidForDays)
	case CertificateActivateData:
		c.RotationID, c.CertificateVersion, c.ExpectedFingerprint = data.RotationID, int64(data.CertificateVersion), data.ExpectedFingerprint
	default:
		return domain.ProbeCertificateCommand{}, domain.ErrValidation
	}
	if !domain.ValidProbeCertificateCommand(c) {
		return domain.ProbeCertificateCommand{}, domain.ErrValidation
	}
	return c, nil
}
