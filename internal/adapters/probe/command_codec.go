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

// AcknowledgementCodec uses the same closed DTO validation as network requests.
type AcknowledgementCodec struct{}

var _ ports.ProbeAcknowledgementCodec = AcknowledgementCodec{}

// EncodeAcknowledgement returns exact command payload bytes, without an envelope.
func (codec AcknowledgementCodec) EncodeAcknowledgement(ctx context.Context, c domain.ProbeAlertAcknowledgement) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	gen, source := Decimal(c.AssignmentGeneration), c.SourceAlertID
	request := CommandRequest{CommandID: c.CommandID, Kind: CommandAlertAck, CreatedAt: Timestamp(c.CreatedAt.UTC()), ExpiresAt: Timestamp(c.ExpiresAt.UTC()), Target: CommandTarget{ProbeID: c.ProbeID, AssignmentGeneration: &gen, SourceAlertID: &source}, Data: AlertAckData{SourceAlertID: source, ActorDisplayName: c.ActorDisplayName, Note: c.Note}}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, domain.ErrValidation
	}
	if _, err := codec.DecodeAcknowledgement(ctx, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// DecodeAcknowledgement validates exact payload bytes and preserves their hash.
func (AcknowledgementCodec) DecodeAcknowledgement(ctx context.Context, payload []byte) (domain.ProbeAlertAcknowledgement, error) {
	if err := ctx.Err(); err != nil {
		return domain.ProbeAlertAcknowledgement{}, err
	}
	if len(payload) == 0 || len(payload) > domain.MaxProbeCommandBytes || !bytes.Equal(payload, bytes.TrimSpace(payload)) {
		return domain.ProbeAlertAcknowledgement{}, domain.ErrValidation
	}
	fields, err := objectFields(payload)
	if err != nil {
		return domain.ProbeAlertAcknowledgement{}, domain.ErrValidation
	}
	request, err := decodeCommandRequestFields(fields)
	if err != nil || request.Kind != CommandAlertAck {
		return domain.ProbeAlertAcknowledgement{}, domain.ErrValidation
	}
	ack, ok := request.Data.(AlertAckData)
	if !ok {
		return domain.ProbeAlertAcknowledgement{}, domain.ErrValidation
	}
	return domain.ProbeAlertAcknowledgement{CommandID: request.CommandID, ProbeID: request.Target.ProbeID, SourceAlertID: ack.SourceAlertID, AssignmentGeneration: int64(*request.Target.AssignmentGeneration), CreatedAt: time.Time(request.CreatedAt).UTC(), ExpiresAt: time.Time(request.ExpiresAt).UTC(), ActorDisplayName: ack.ActorDisplayName, Note: ack.Note, PayloadHash: sha256.Sum256(payload)}, nil
}
