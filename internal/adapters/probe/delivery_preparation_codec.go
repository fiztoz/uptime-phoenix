package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// DeliveryPreparationCapability advertises durable delivery-mode preparation
// requests only. It never promises ModeApplicationSupported, mode application,
// aggregate sends, suppression, a cutover barrier or a send permit. Existing
// regional report negotiation is a separate, independently required gate.
const DeliveryPreparationCapability = "command.delivery_prepare.v1"

// DeliveryPreparationCodec maps the closed wire request to bounded core data.
type DeliveryPreparationCodec struct{}

var _ ports.ProbeDeliveryPreparationCodec = DeliveryPreparationCodec{}

// EncodeDeliveryPreparation returns exact command payload bytes, without an
// envelope. The caller's PayloadHash is never authoritative and may be zero:
// identity is bound from these canonical bytes by self-decoding the output.
func (codec DeliveryPreparationCodec) EncodeDeliveryPreparation(ctx context.Context, c domain.ProbeDeliveryPreparation) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	gen := Decimal(c.Generation)
	request := CommandRequest{CommandID: c.CommandID, Kind: CommandDeliveryPrepare, CreatedAt: Timestamp(c.CreatedAt.UTC()), ExpiresAt: Timestamp(c.ExpiresAt.UTC()), Target: CommandTarget{ProbeID: c.ProbeID, AssignmentGeneration: &gen, SourceAlertID: nil}, Data: DeliveryPreparationData{HubID: c.HubID, StreamID: c.StreamID, MonitorID: c.MonitorID, AssignmentRevision: Decimal(c.AssignmentRevision), ConfigRevision: Decimal(c.ConfigRevision), SHA256: c.SHA256, AlertDelivery: c.AlertDelivery}}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, domain.ErrValidation
	}
	decoded, err := codec.DecodeDeliveryPreparation(ctx, payload)
	if err != nil {
		return nil, err
	}
	want := c
	want.CreatedAt, want.ExpiresAt, want.PayloadHash = c.CreatedAt.UTC(), c.ExpiresAt.UTC(), decoded.PayloadHash
	if !sameDeliveryPreparationRequest(decoded, want) {
		return nil, domain.ErrValidation
	}
	return payload, nil
}

// DecodeDeliveryPreparation validates exact payload bytes, never a session
// frame, and binds their SHA-256 to the bounded request. It decodes a REQUEST
// whose mode is a preparation prerequisite only; the result is never effective
// application evidence and carries no ModeApplicationSupported claim.
func (DeliveryPreparationCodec) DecodeDeliveryPreparation(ctx context.Context, payload []byte) (domain.ProbeDeliveryPreparation, error) {
	if err := ctx.Err(); err != nil {
		return domain.ProbeDeliveryPreparation{}, err
	}
	if len(payload) == 0 || len(payload) > domain.MaxProbeCommandBytes || !bytes.Equal(payload, bytes.TrimSpace(payload)) {
		return domain.ProbeDeliveryPreparation{}, domain.ErrValidation
	}
	fields, err := decodeJSONObject(payload)
	if err != nil {
		return domain.ProbeDeliveryPreparation{}, domain.ErrValidation
	}
	request, err := decodeCommandRequestFields(fields)
	if err != nil {
		if errors.Is(err, ErrUnsupportedPayload) {
			return domain.ProbeDeliveryPreparation{}, err
		}
		return domain.ProbeDeliveryPreparation{}, domain.ErrValidation
	}
	if request.Kind != CommandDeliveryPrepare {
		return domain.ProbeDeliveryPreparation{}, domain.ErrValidation
	}
	data, ok := request.Data.(DeliveryPreparationData)
	if !ok {
		return domain.ProbeDeliveryPreparation{}, domain.ErrValidation
	}
	preparation := domain.ProbeDeliveryPreparation{
		ProbeDeliveryModeApplication: domain.ProbeDeliveryModeApplication{
			ProbeConfigTarget:  domain.ProbeConfigTarget{HubID: data.HubID, ProbeID: request.Target.ProbeID},
			MonitorID:          data.MonitorID,
			AssignmentRevision: int64(data.AssignmentRevision),
			Generation:         int64(*request.Target.AssignmentGeneration),
			ConfigRevision:     int64(data.ConfigRevision),
			SHA256:             data.SHA256,
			StreamID:           data.StreamID,
			AlertDelivery:      data.AlertDelivery,
		},
		CommandID:   request.CommandID,
		CreatedAt:   time.Time(request.CreatedAt).UTC(),
		ExpiresAt:   time.Time(request.ExpiresAt).UTC(),
		PayloadHash: sha256.Sum256(payload),
	}
	if !domain.ValidProbeDeliveryPreparation(preparation) {
		return domain.ProbeDeliveryPreparation{}, domain.ErrValidation
	}
	return preparation, nil
}

// sameDeliveryPreparationRequest compares one request binding, treating a zero
// PayloadHash as freshly bound and timestamps as instants.
func sameDeliveryPreparationRequest(got, want domain.ProbeDeliveryPreparation) bool {
	return got.ProbeDeliveryModeApplication == want.ProbeDeliveryModeApplication &&
		got.CommandID == want.CommandID &&
		got.CreatedAt.Equal(want.CreatedAt) && got.ExpiresAt.Equal(want.ExpiresAt) &&
		(want.PayloadHash == [32]byte{} || want.PayloadHash == got.PayloadHash)
}
