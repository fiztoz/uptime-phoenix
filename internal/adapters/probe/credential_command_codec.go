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

// CredentialCommandCodec shares closed DTO validation with the runtime adapter.
type CredentialCommandCodec struct{}

var _ ports.ProbeCredentialCommandCodec = CredentialCommandCodec{}

// EncodeCredentialCommand encodes a write-only token in an exact bounded request.
func (codec CredentialCommandCodec) EncodeCredentialCommand(ctx context.Context, c domain.ProbeCredentialCommand, token string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request := CommandRequest{CommandID: c.CommandID, Kind: c.Kind, CreatedAt: Timestamp(c.CreatedAt.UTC()), ExpiresAt: Timestamp(c.ExpiresAt.UTC()), Target: CommandTarget{ProbeID: c.ProbeID}}
	switch c.Kind {
	case CommandCredentialPrepare:
		if sha256.Sum256([]byte(token)) != c.TokenHash {
			return nil, domain.ErrValidation
		}
		request.Data = CredentialPrepareData{RotationID: c.RotationID, CredentialVersion: Decimal(c.CredentialVersion), Token: token, OverlapExpiresAt: Timestamp(c.OverlapExpiresAt.UTC())}
	case CommandCredentialActivate:
		if token != "" || c.TokenHash != [32]byte{} || !c.OverlapExpiresAt.IsZero() {
			return nil, domain.ErrValidation
		}
		request.Data = CredentialActivateData{RotationID: c.RotationID, CredentialVersion: Decimal(c.CredentialVersion)}
	default:
		return nil, domain.ErrValidation
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, domain.ErrValidation
	}
	if _, err := codec.DecodeCredentialCommand(ctx, payload); err != nil {
		clear(payload)
		return nil, err
	}
	return payload, nil
}

// DecodeCredentialCommand validates exact bytes and retains only the token digest.
func (CredentialCommandCodec) DecodeCredentialCommand(ctx context.Context, payload []byte) (domain.ProbeCredentialCommand, error) {
	if err := ctx.Err(); err != nil {
		return domain.ProbeCredentialCommand{}, err
	}
	if len(payload) == 0 || len(payload) > domain.MaxProbeCommandBytes || !bytes.Equal(payload, bytes.TrimSpace(payload)) {
		return domain.ProbeCredentialCommand{}, domain.ErrValidation
	}
	fields, err := decodeJSONObject(payload)
	if err != nil {
		return domain.ProbeCredentialCommand{}, domain.ErrValidation
	}
	request, err := decodeCommandRequestFields(fields)
	if err != nil {
		return domain.ProbeCredentialCommand{}, domain.ErrValidation
	}
	c := domain.ProbeCredentialCommand{CommandID: request.CommandID, ProbeID: request.Target.ProbeID, Kind: request.Kind, CreatedAt: time.Time(request.CreatedAt).UTC(), ExpiresAt: time.Time(request.ExpiresAt).UTC(), PayloadHash: sha256.Sum256(payload)}
	switch data := request.Data.(type) {
	case CredentialPrepareData:
		c.RotationID, c.CredentialVersion, c.TokenHash, c.OverlapExpiresAt = data.RotationID, int64(data.CredentialVersion), sha256.Sum256([]byte(data.Token)), time.Time(data.OverlapExpiresAt).UTC()
	case CredentialActivateData:
		c.RotationID, c.CredentialVersion = data.RotationID, int64(data.CredentialVersion)
	default:
		return domain.ProbeCredentialCommand{}, domain.ErrValidation
	}
	if !domain.ValidProbeCredentialCommand(c) {
		return domain.ProbeCredentialCommand{}, domain.ErrValidation
	}
	return c, nil
}
