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

// DeliveryCutoverCapability is the exact strong capability of a source runtime
// with a real ports.EdgeDeliveryCutoverExecutor configured. It promises
// protected source-gate cutover command execution only: no aggregate send,
// suppression, activation token, ModeApplicationSupported claim or mode
// application authority. It never substitutes for, and is never substituted by,
// delivery preparation or regional application reporting, which keep their own
// narrower meanings.
const DeliveryCutoverCapability = domain.DeliveryCutoverCapability

// DeliveryCutoverCodec maps the closed protected cutover request to bounded
// pure core data. The request is a preparation tuple plus the immutable barrier
// and preparation command identity; its kind names exactly one source-gate
// effect. Shape only: negotiated support, stored authority and runtime behavior
// are checked separately before execution.
type DeliveryCutoverCodec struct{}

var _ ports.DeliveryCutoverCodec = DeliveryCutoverCodec{}

// EncodeDeliveryCutover returns exact command payload bytes, without an
// envelope. The caller's PayloadHash is never authoritative and may be zero:
// identity is bound from these canonical bytes by self-decoding the output.
func (codec DeliveryCutoverCodec) EncodeDeliveryCutover(ctx context.Context, c domain.DeliveryCutoverCommand) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	gen := Decimal(c.Generation)
	request := CommandRequest{CommandID: c.CommandID, Kind: c.Kind, CreatedAt: Timestamp(c.CreatedAt.UTC()), ExpiresAt: Timestamp(c.ExpiresAt.UTC()), Target: CommandTarget{ProbeID: c.ProbeID, AssignmentGeneration: &gen, SourceAlertID: nil}, Data: DeliveryCutoverData{HubID: c.HubID, StreamID: c.StreamID, MonitorID: c.MonitorID, AssignmentRevision: Decimal(c.AssignmentRevision), ConfigRevision: Decimal(c.ConfigRevision), SHA256: c.SHA256, AlertDelivery: c.AlertDelivery, BarrierID: c.BarrierID, PreparationCommandID: c.PreparationCommandID}}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, domain.ErrValidation
	}
	decoded, err := codec.DecodeDeliveryCutover(ctx, payload)
	if err != nil {
		return nil, err
	}
	want := c
	want.CreatedAt, want.ExpiresAt, want.PayloadHash = c.CreatedAt.UTC(), c.ExpiresAt.UTC(), decoded.PayloadHash
	if !sameDeliveryCutoverCommand(decoded, want) {
		return nil, domain.ErrValidation
	}
	return payload, nil
}

// DecodeDeliveryCutover validates exact payload bytes, never a session frame,
// and binds their SHA-256 to the bounded command. It decodes one protected
// source-gate effect request; the resulting outcome is produced only by the
// configured executor and is never implied here.
func (DeliveryCutoverCodec) DecodeDeliveryCutover(ctx context.Context, payload []byte) (domain.DeliveryCutoverCommand, error) {
	if err := ctx.Err(); err != nil {
		return domain.DeliveryCutoverCommand{}, err
	}
	if len(payload) == 0 || len(payload) > domain.MaxProbeCommandBytes || !bytes.Equal(payload, bytes.TrimSpace(payload)) {
		return domain.DeliveryCutoverCommand{}, domain.ErrValidation
	}
	fields, err := decodeJSONObject(payload)
	if err != nil {
		return domain.DeliveryCutoverCommand{}, domain.ErrValidation
	}
	request, err := decodeCommandRequestFields(fields)
	if err != nil {
		if errors.Is(err, ErrUnsupportedPayload) {
			return domain.DeliveryCutoverCommand{}, err
		}
		return domain.DeliveryCutoverCommand{}, domain.ErrValidation
	}
	if request.Kind != CommandDeliveryQuiesce && request.Kind != CommandDeliveryApply && request.Kind != CommandDeliveryResume {
		return domain.DeliveryCutoverCommand{}, domain.ErrValidation
	}
	data, ok := request.Data.(DeliveryCutoverData)
	if !ok || request.Target.AssignmentGeneration == nil {
		return domain.DeliveryCutoverCommand{}, domain.ErrValidation
	}
	command := domain.DeliveryCutoverCommand{
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
		CommandID:            request.CommandID,
		BarrierID:            data.BarrierID,
		PreparationCommandID: data.PreparationCommandID,
		Kind:                 request.Kind,
		CreatedAt:            time.Time(request.CreatedAt).UTC(),
		ExpiresAt:            time.Time(request.ExpiresAt).UTC(),
		PayloadHash:          sha256.Sum256(payload),
	}
	// Canonical hub control bounds: the reserved local stream never names a
	// remote source command (the shared validator covers ProbeID), and sealed
	// metadata carries the hub storage's microsecond precision.
	if command.StreamID == domain.LocalStreamID || command.CreatedAt.Nanosecond()%1000 != 0 || command.ExpiresAt.Nanosecond()%1000 != 0 {
		return domain.DeliveryCutoverCommand{}, domain.ErrValidation
	}
	if !domain.ValidDeliveryCutoverCommand(command) {
		return domain.DeliveryCutoverCommand{}, domain.ErrValidation
	}
	return command, nil
}

// sameDeliveryCutoverCommand compares one request binding, treating a zero
// PayloadHash as freshly bound and timestamps as instants.
func sameDeliveryCutoverCommand(got, want domain.DeliveryCutoverCommand) bool {
	return got.ProbeDeliveryModeApplication == want.ProbeDeliveryModeApplication &&
		got.CommandID == want.CommandID && got.BarrierID == want.BarrierID &&
		got.PreparationCommandID == want.PreparationCommandID && got.Kind == want.Kind &&
		got.CreatedAt.Equal(want.CreatedAt) && got.ExpiresAt.Equal(want.ExpiresAt) &&
		(want.PayloadHash == [32]byte{} || want.PayloadHash == got.PayloadHash)
}
