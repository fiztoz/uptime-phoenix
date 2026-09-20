package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// AcknowledgementCapability advertises durable source ACK execution, not decoding.
const AcknowledgementCapability = "command.alert_ack.v1"

// CredentialRotationCapability advertises source rotation with bounded authentication.
const CredentialRotationCapability = "command.credential_rotation.v1"

func (t *HubTransport) sendCommands(ctx context.Context, session *Session, authority domain.ProbeReplaySession, ready *atomic.Bool) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if ready.Load() {
			opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			command, err := t.commands.NextCommand(opCtx, authority, 5*time.Second)
			if err == nil && command != nil {
				frame, encodeErr := encodeCommandRequestFrame(Decimal(authority.ConnectionGeneration), command.Payload)
				clear(command.Payload)
				err = encodeErr
				if err == nil {
					err = session.SendControl(opCtx, frame)
				}
			}
			cancel()
			if err != nil {
				_ = session.Close()
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// A normal json.Marshal compacts RawMessage and can change immutable request
// identity. Replace only the known empty payload slot, then verify the exact
// decoded value before permitting the frame to leave this process.
func encodeCommandRequestFrame(generation Decimal, payload []byte) ([]byte, error) {
	if len(payload) == 0 || len(payload) > domain.MaxProbeCommandBytes || !json.Valid(payload) {
		return nil, domain.ErrValidation
	}
	header, err := encodeFrame("command.request", generation, nil)
	if err != nil || !bytes.HasSuffix(header, []byte(`"payload":null}`)) {
		return nil, domain.ErrValidation
	}
	frame := append(header[:len(header)-len("null}")], payload...)
	frame = append(frame, '}')
	envelope, _, err := DecodeCommandRequest(frame)
	if err != nil || !bytes.Equal(envelope.Payload, payload) {
		return nil, domain.ErrValidation
	}
	return frame, nil
}

func applyEdgeCommand(ctx context.Context, repo ports.EdgeCommandRepository, credentials ports.EdgeCredentialRepository, authority domain.EdgeCommandAuthority, envelope Envelope) ([]byte, bool, error) {
	if len(envelope.Payload) == 0 || len(envelope.Payload) > domain.MaxProbeCommandBytes || !bytes.Equal(envelope.Payload, bytes.TrimSpace(envelope.Payload)) {
		return nil, false, errors.New("invalid command request")
	}
	fields, err := objectFields(envelope.Payload)
	if err != nil {
		return nil, false, errors.New("invalid command request")
	}
	request, err := decodeCommandRequestFields(fields)
	if err != nil {
		return nil, false, errors.New("invalid command request")
	}
	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var result domain.ProbeCommandOutcome
	reconnect := false
	switch request.Kind {
	case CommandAlertAck:
		if repo == nil {
			return nil, false, errors.New("command execution unavailable")
		}
		command, decodeErr := (AcknowledgementCodec{}).DecodeAcknowledgement(ctx, envelope.Payload)
		if decodeErr != nil {
			return nil, false, errors.New("invalid command request")
		}
		result, err = repo.ApplyAlertAcknowledgement(opCtx, authority, command)
	case CommandCredentialPrepare, CommandCredentialActivate:
		if credentials == nil {
			return nil, false, errors.New("credential rotation unavailable")
		}
		c := domain.ProbeCredentialCommand{CommandID: request.CommandID, ProbeID: request.Target.ProbeID, Kind: request.Kind, CreatedAt: time.Time(request.CreatedAt).UTC(), ExpiresAt: time.Time(request.ExpiresAt).UTC(), PayloadHash: sha256.Sum256(envelope.Payload)}
		switch data := request.Data.(type) {
		case CredentialPrepareData:
			c.RotationID, c.CredentialVersion, c.TokenHash, c.OverlapExpiresAt = data.RotationID, int64(data.CredentialVersion), sha256.Sum256([]byte(data.Token)), time.Time(data.OverlapExpiresAt).UTC()
		case CredentialActivateData:
			c.RotationID, c.CredentialVersion = data.RotationID, int64(data.CredentialVersion)
		default:
			return nil, false, errors.New("invalid credential rotation")
		}
		result, err = credentials.ApplyCredentialCommand(opCtx, authority, c)
		reconnect = result.Status == "applied" || result.Status == "already_applied"
	default:
		return nil, false, errors.New("unsupported command request")
	}
	if err != nil {
		return nil, false, errors.New("command application unavailable")
	}

	var at *Timestamp
	if result.AppliedAt != nil {
		v := Timestamp(result.AppliedAt.UTC())
		at = &v
	}
	var code *string
	if result.Code != "" {
		code = &result.Code
	}
	var details CommandResultDetails
	if result.CredentialVersion > 0 {
		details = CredentialPrepareDetails{CredentialVersion: Decimal(result.CredentialVersion)}
	}
	frame, err := encodeFrame("command.result", envelope.ConnectionGeneration, CommandResult{CommandID: result.CommandID, Status: result.Status, AppliedAt: at, Code: code, Message: result.Message, Details: details})
	if err != nil {
		return nil, false, err
	}
	if _, _, err := DecodeCommandResult(frame); err != nil {
		return nil, false, errors.New("invalid durable command result")
	}
	return frame, reconnect, nil
}

func commandOutcome(result CommandResult) (domain.ProbeCommandOutcome, error) {
	if result.Details != nil {
		return domain.ProbeCommandOutcome{}, errors.New("unexpected ACK result details")
	}
	out := domain.ProbeCommandOutcome{CommandID: result.CommandID, Status: result.Status, Message: result.Message}
	if result.AppliedAt != nil {
		at := time.Time(*result.AppliedAt).UTC()
		out.AppliedAt = &at
	}
	if result.Code != nil {
		out.Code = *result.Code
	}
	return out, nil
}
