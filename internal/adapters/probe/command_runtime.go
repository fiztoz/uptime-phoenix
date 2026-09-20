package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// AcknowledgementCapability advertises durable source ACK execution, not decoding.
const AcknowledgementCapability = "command.alert_ack.v1"

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

func applyEdgeCommand(ctx context.Context, repo ports.EdgeCommandRepository, authority domain.EdgeCommandAuthority, envelope Envelope) ([]byte, error) {
	if repo == nil {
		return nil, errors.New("command execution unavailable")
	}
	command, err := (AcknowledgementCodec{}).DecodeAcknowledgement(ctx, envelope.Payload)
	if err != nil {
		return nil, errors.New("unsupported or invalid command request")
	}
	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	result, err := repo.ApplyAlertAcknowledgement(opCtx, authority, command)
	cancel()
	if err != nil {
		return nil, errors.New("command application unavailable")
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
	frame, err := encodeFrame("command.result", envelope.ConnectionGeneration, CommandResult{CommandID: result.CommandID, Status: result.Status, AppliedAt: at, Code: code, Message: result.Message})
	if err != nil {
		return nil, err
	}
	if _, _, err := DecodeCommandResult(frame); err != nil {
		return nil, errors.New("invalid durable command result")
	}
	return frame, nil
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
