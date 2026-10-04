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

// CredentialRotationCapability advertises source rotation with bounded authentication.
const CredentialRotationCapability = "command.credential_rotation.v1"

// CertificateRotationCapability advertises durable local TLS certificate switching.
const CertificateRotationCapability = "command.certificate_rotation.v1"

func (t *HubTransport) sendCommands(ctx context.Context, session *Session, authority domain.ProbeReplaySession, ready *atomic.Bool, capabilities domain.ProbeCommandCapabilities) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if ready.Load() {
			opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			command, err := t.commands.NextCommand(opCtx, authority, 5*time.Second, capabilities)
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

type edgeCommandReconnect struct {
	required                  bool
	deadline                  time.Time
	preparedFingerprint       string
	preparedCredentialVersion int64
}

func (r edgeCommandReconnect) delay(binding domain.EdgeEnrollment) time.Duration {
	grace := 12 * time.Second
	// A historical prepare receipt does not retire the current new identity.
	// The admission context independently enforces any existing session deadline.
	if r.deadline.IsZero() || (r.preparedFingerprint != "" && r.preparedFingerprint == binding.CertificateFingerprint) || (r.preparedCredentialVersion > 0 && r.preparedCredentialVersion == binding.CredentialVersion) {
		return grace
	}
	return min(grace, time.Until(r.deadline))
}

func applyEdgeCommand(ctx context.Context, repo ports.EdgeCommandRepository, credentials ports.EdgeCredentialRepository, certificates ports.EdgeCertificateRepository, authority domain.EdgeCommandAuthority, envelope Envelope) ([]byte, edgeCommandReconnect, error) {
	if len(envelope.Payload) == 0 || len(envelope.Payload) > domain.MaxProbeCommandBytes || !bytes.Equal(envelope.Payload, bytes.TrimSpace(envelope.Payload)) {
		return nil, edgeCommandReconnect{}, errors.New("invalid command request")
	}
	fields, err := objectFields(envelope.Payload)
	if err != nil {
		return nil, edgeCommandReconnect{}, errors.New("invalid command request")
	}
	request, err := decodeCommandRequestFields(fields)
	if err != nil {
		return nil, edgeCommandReconnect{}, errors.New("invalid command request")
	}
	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var result domain.ProbeCommandOutcome
	var reconnect edgeCommandReconnect
	started := time.Now().UTC()
	switch request.Kind {
	case CommandAlertAck:
		if repo == nil {
			return nil, edgeCommandReconnect{}, errors.New("command execution unavailable")
		}
		command, decodeErr := (AcknowledgementCodec{}).DecodeAcknowledgement(ctx, envelope.Payload)
		if decodeErr != nil {
			return nil, edgeCommandReconnect{}, errors.New("invalid command request")
		}
		result, err = repo.ApplyAlertAcknowledgement(opCtx, authority, command)
	case CommandCredentialPrepare, CommandCredentialActivate:
		if credentials == nil {
			return nil, edgeCommandReconnect{}, errors.New("credential rotation unavailable")
		}
		c, decodeErr := (CredentialCommandCodec{}).DecodeCredentialCommand(ctx, envelope.Payload)
		if decodeErr != nil {
			return nil, edgeCommandReconnect{}, errors.New("invalid credential rotation")
		}
		result, err = credentials.ApplyCredentialCommand(opCtx, authority, c)
		if c.Kind == CommandCredentialPrepare && started.Before(c.OverlapExpiresAt) {
			reconnect.deadline = c.OverlapExpiresAt
			reconnect.preparedCredentialVersion = c.CredentialVersion
		}
		reconnect.required = result.Status == "applied" || result.Status == "already_applied"
	case CommandCertificatePrepare, CommandCertificateActivate:
		if certificates == nil {
			return nil, edgeCommandReconnect{}, errors.New("certificate rotation unavailable")
		}
		c, decodeErr := (CertificateCommandCodec{}).DecodeCertificateCommand(ctx, envelope.Payload)
		if decodeErr != nil {
			return nil, edgeCommandReconnect{}, errors.New("invalid certificate rotation")
		}
		result, err = certificates.ApplyCertificateCommand(opCtx, authority, c)
		if c.Kind == CommandCertificatePrepare && started.Before(c.CreatedAt.Add(domain.ProbeCredentialOverlap)) {
			reconnect.deadline = c.CreatedAt.Add(domain.ProbeCredentialOverlap)
			reconnect.preparedFingerprint = result.CertificateFingerprint
		}
		reconnect.required = result.Status == "applied" || result.Status == "already_applied"
	default:
		return nil, edgeCommandReconnect{}, errors.New("unsupported command request")
	}
	if err != nil {
		return nil, edgeCommandReconnect{}, errors.New("command application unavailable")
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
	if result.CertificateVersion > 0 {
		if details != nil || result.CertificateNotAfter == nil {
			return nil, edgeCommandReconnect{}, errors.New("invalid certificate result details")
		}
		details = CertificatePrepareDetails{CertificateVersion: Decimal(result.CertificateVersion), TLSFingerprint: result.CertificateFingerprint, NotAfter: Timestamp(result.CertificateNotAfter.UTC())}
	}
	frame, err := encodeFrame("command.result", envelope.ConnectionGeneration, CommandResult{CommandID: result.CommandID, Status: result.Status, AppliedAt: at, Code: code, Message: result.Message, Details: details})
	if err != nil {
		return nil, edgeCommandReconnect{}, err
	}
	if _, _, err := DecodeCommandResult(frame); err != nil {
		return nil, edgeCommandReconnect{}, errors.New("invalid durable command result")
	}
	return frame, reconnect, nil
}

func commandOutcome(result CommandResult) (domain.ProbeCommandOutcome, error) {
	out := domain.ProbeCommandOutcome{CommandID: result.CommandID, Status: result.Status, Message: result.Message}
	if result.Details != nil {
		switch details := result.Details.(type) {
		case CredentialPrepareDetails:
			out.CredentialVersion = int64(details.CredentialVersion)
		case CertificatePrepareDetails:
			out.CertificateVersion = int64(details.CertificateVersion)
			out.CertificateFingerprint = details.TLSFingerprint
			expiry := time.Time(details.NotAfter).UTC()
			out.CertificateNotAfter = &expiry
		default:
			return domain.ProbeCommandOutcome{}, errors.New("unsupported command result details")
		}
	}
	if result.AppliedAt != nil {
		at := time.Time(*result.AppliedAt).UTC()
		out.AppliedAt = &at
	}
	if result.Code != nil {
		out.Code = *result.Code
	}
	return out, nil
}
