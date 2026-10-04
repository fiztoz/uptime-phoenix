package probe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	// MaxCertificateValidDays bounds certificate.prepare lifetime requests.
	MaxCertificateValidDays = 3650
)

// Command kinds documented in PROTOCOL.md section 6.1.
const (
	CommandAlertAck            = "alert.ack"
	CommandProbeStop           = "probe.stop"
	CommandHistoryClear        = "history.clear"
	CommandCredentialPrepare   = "credential.prepare"
	CommandCredentialActivate  = "credential.activate"
	CommandCertificatePrepare  = "certificate.prepare"
	CommandCertificateActivate = "certificate.activate"
)

// CommandRequest is a hub-issued instruction. Decoding does not execute it.
type CommandRequest struct {
	CommandID string        `json:"command_id"`
	Kind      string        `json:"kind"`
	CreatedAt Timestamp     `json:"created_at"`
	ExpiresAt Timestamp     `json:"expires_at"`
	Target    CommandTarget `json:"target"`
	Data      CommandData   `json:"data"`
}

// CommandTarget identifies the probe and, when applicable, assignment and incident.
type CommandTarget struct {
	ProbeID              string   `json:"probe_id"`
	AssignmentGeneration *Decimal `json:"assignment_generation"`
	SourceAlertID        *string  `json:"source_alert_id"`
}

// CommandData is the closed per-kind payload.
type CommandData interface {
	isCommandData()
}

// AlertAckData acknowledges one source incident.
type AlertAckData struct {
	SourceAlertID    string  `json:"source_alert_id"`
	ActorDisplayName string  `json:"actor_display_name"`
	Note             *string `json:"note"`
}

// ProbeStopData durably pauses scheduling after accepted shutdown instructions.
type ProbeStopData struct {
	Reason      string    `json:"reason"`
	EffectiveAt Timestamp `json:"effective_at"`
}

// HistoryClearData discards evidence through an explicit watermark.
type HistoryClearData struct {
	MonitorID            int64     `json:"monitor_id"`
	AssignmentGeneration Decimal   `json:"assignment_generation"`
	ThroughObservedAt    Timestamp `json:"through_observed_at"`
	ThroughSeq           Decimal   `json:"through_seq"`
	ClearID              string    `json:"clear_id"`
}

// CredentialPrepareData offers a write-only runtime credential.
type CredentialPrepareData struct {
	RotationID        string    `json:"rotation_id"`
	CredentialVersion Decimal   `json:"credential_version"`
	Token             string    `json:"token"`
	OverlapExpiresAt  Timestamp `json:"overlap_expires_at"`
}

// CredentialActivateData activates a previously prepared credential version.
type CredentialActivateData struct {
	RotationID        string  `json:"rotation_id"`
	CredentialVersion Decimal `json:"credential_version"`
}

// CertificatePrepareData asks the probe to mint a pending certificate locally.
type CertificatePrepareData struct {
	RotationID         string  `json:"rotation_id"`
	CertificateVersion Decimal `json:"certificate_version"`
	ValidForDays       int64   `json:"valid_for_days"`
}

// CertificateActivateData activates a prepared certificate by expected pin.
type CertificateActivateData struct {
	RotationID          string  `json:"rotation_id"`
	CertificateVersion  Decimal `json:"certificate_version"`
	ExpectedFingerprint string  `json:"expected_fingerprint"`
}

func (AlertAckData) isCommandData()            {}
func (ProbeStopData) isCommandData()           {}
func (HistoryClearData) isCommandData()        {}
func (CredentialPrepareData) isCommandData()   {}
func (CredentialActivateData) isCommandData()  {}
func (CertificatePrepareData) isCommandData()  {}
func (CertificateActivateData) isCommandData() {}

// CommandResult is a persisted outcome shape, not proof that a command ran.
type CommandResult struct {
	CommandID string               `json:"command_id"`
	Status    string               `json:"status"`
	AppliedAt *Timestamp           `json:"applied_at"`
	Code      *string              `json:"code"`
	Message   string               `json:"message"`
	Details   CommandResultDetails `json:"details"`
}

// CommandResultDetails is a closed prepare receipt. It never carries secrets.
type CommandResultDetails interface {
	isCommandResultDetails()
}

// CredentialPrepareDetails reports only the prepared credential version.
type CredentialPrepareDetails struct {
	CredentialVersion Decimal `json:"credential_version"`
}

// CertificatePrepareDetails reports the locally minted certificate identity.
type CertificatePrepareDetails struct {
	CertificateVersion Decimal   `json:"certificate_version"`
	TLSFingerprint     string    `json:"tls_fingerprint"`
	NotAfter           Timestamp `json:"not_after"`
}

func (CredentialPrepareDetails) isCommandResultDetails()  {}
func (CertificatePrepareDetails) isCommandResultDetails() {}

// DecodeCommandRequest validates a command.request payload. It does not apply it.
func DecodeCommandRequest(data []byte) (Envelope, CommandRequest, error) {
	envelope, fields, err := telemetryFields(data, "command.request")
	if err != nil {
		return envelope, CommandRequest{}, err
	}
	command, err := decodeCommandRequestFields(fields)
	return envelope, command, err
}

func decodeCommandRequestFields(fields map[string]json.RawMessage) (CommandRequest, error) {
	var command CommandRequest
	var err error
	if err := requiredUUID(fields, "command_id", &command.CommandID); err != nil {
		return CommandRequest{}, err
	}
	if err := decodeRequiredFields(fields,
		field{"kind", &command.Kind}, field{"created_at", &command.CreatedAt}, field{"expires_at", &command.ExpiresAt},
	); err != nil {
		return CommandRequest{}, err
	}
	var targetRaw json.RawMessage
	if err := required(fields, "target", &targetRaw); err != nil {
		return CommandRequest{}, err
	}
	command.Target, err = decodeCommandTarget(targetRaw)
	if err != nil {
		return CommandRequest{}, err
	}
	var dataRaw json.RawMessage
	if err := required(fields, "data", &dataRaw); err != nil {
		return CommandRequest{}, err
	}
	command.Data, err = decodeCommandData(command.Kind, dataRaw, command.Target)
	if err != nil {
		return CommandRequest{}, err
	}
	return command, nil
}

// DecodeCommandResult validates a command.result payload without claiming execution.
func DecodeCommandResult(data []byte) (Envelope, CommandResult, error) {
	var result CommandResult
	envelope, fields, err := telemetryFields(data, "command.result")
	if err != nil {
		return envelope, CommandResult{}, err
	}
	if err := rejectSecretPayload(envelope.Payload); err != nil {
		return envelope, CommandResult{}, err
	}
	if err := requiredUUID(fields, "command_id", &result.CommandID); err != nil {
		return envelope, CommandResult{}, err
	}
	if err := required(fields, "status", &result.Status); err != nil {
		return envelope, CommandResult{}, err
	}
	if err := nullable(fields, "applied_at", &result.AppliedAt); err != nil {
		return envelope, CommandResult{}, err
	}
	if err := nullable(fields, "code", &result.Code); err != nil {
		return envelope, CommandResult{}, err
	}
	if err := required(fields, "message", &result.Message); err != nil {
		return envelope, CommandResult{}, err
	}
	if err := validRedactedMessage(result.Message); err != nil {
		return envelope, CommandResult{}, err
	}
	if result.Code != nil && !validErrorCode(*result.Code) {
		return envelope, CommandResult{}, errors.New("invalid command result code")
	}
	var detailsRaw *json.RawMessage
	if err := nullable(fields, "details", &detailsRaw); err != nil {
		return envelope, CommandResult{}, err
	}
	switch result.Status {
	case "applied", "already_applied", "already_resolved":
		if result.AppliedAt == nil || result.Code != nil {
			return envelope, CommandResult{}, errors.New("successful command result requires applied_at and null code")
		}
	case "rejected":
		if result.AppliedAt != nil || result.Code == nil {
			return envelope, CommandResult{}, errors.New("rejected command result requires a code and null applied_at")
		}
	case "expired":
		if result.AppliedAt != nil {
			return envelope, CommandResult{}, errors.New("expired command result cannot include applied_at")
		}
	default:
		return envelope, CommandResult{}, errors.New("invalid command result status")
	}
	if detailsRaw != nil {
		if result.Status != "applied" && result.Status != "already_applied" {
			return envelope, CommandResult{}, errors.New("command result details are limited to applied preparation")
		}
		details, err := decodeCommandResultDetails(*detailsRaw)
		if err != nil {
			return envelope, CommandResult{}, err
		}
		result.Details = details
	}
	return envelope, result, nil
}

func decodeCommandTarget(data []byte) (CommandTarget, error) {
	var target CommandTarget
	fields, err := objectFields(data)
	if err != nil {
		return CommandTarget{}, err
	}
	if err := rejectUnknownKeys(fields, "probe_id", "assignment_generation", "source_alert_id"); err != nil {
		return CommandTarget{}, err
	}
	if err := requiredUUID(fields, "probe_id", &target.ProbeID); err != nil {
		return CommandTarget{}, err
	}
	if err := nullable(fields, "assignment_generation", &target.AssignmentGeneration); err != nil {
		return CommandTarget{}, err
	}
	if err := nullable(fields, "source_alert_id", &target.SourceAlertID); err != nil {
		return CommandTarget{}, err
	}
	if target.AssignmentGeneration != nil && *target.AssignmentGeneration <= 0 {
		return CommandTarget{}, errors.New("assignment_generation must be positive when present")
	}
	if target.SourceAlertID != nil {
		if err := canonicalUUID("source_alert_id", *target.SourceAlertID); err != nil {
			return CommandTarget{}, err
		}
	}
	return target, nil
}

func decodeCommandData(kind string, data []byte, target CommandTarget) (CommandData, error) {
	fields, err := objectFields(data)
	if err != nil {
		return nil, err
	}
	switch kind {
	case CommandAlertAck:
		if target.AssignmentGeneration == nil || target.SourceAlertID == nil {
			return nil, errors.New("alert.ack requires assignment generation and source incident")
		}
		if err := rejectUnknownKeys(fields, "source_alert_id", "actor_display_name", "note"); err != nil {
			return nil, err
		}
		var ack AlertAckData
		if err := requiredUUID(fields, "source_alert_id", &ack.SourceAlertID); err != nil {
			return nil, err
		}
		if ack.SourceAlertID != *target.SourceAlertID {
			return nil, errors.New("alert.ack target and data incident disagree")
		}
		if err := required(fields, "actor_display_name", &ack.ActorDisplayName); err != nil {
			return nil, err
		}
		if err := nullable(fields, "note", &ack.Note); err != nil {
			return nil, err
		}
		if strings.TrimSpace(ack.ActorDisplayName) == "" || len(ack.ActorDisplayName) > MaxMetadataBytes {
			return nil, errors.New("invalid acknowledgement actor length")
		}
		if ack.Note != nil && len(*ack.Note) > MaxMessageBytes {
			return nil, errors.New("acknowledgement note exceeds maximum bytes")
		}
		if ack.Note != nil {
			if err := validRedactedMessage(*ack.Note); err != nil {
				return nil, err
			}
		}
		return ack, nil
	case CommandProbeStop:
		if target.AssignmentGeneration != nil || target.SourceAlertID != nil {
			return nil, errors.New("probe.stop cannot target an assignment or incident")
		}
		if err := rejectUnknownKeys(fields, "reason", "effective_at"); err != nil {
			return nil, err
		}
		var stop ProbeStopData
		if err := decodeRequiredFields(fields, field{"reason", &stop.Reason}, field{"effective_at", &stop.EffectiveAt}); err != nil {
			return nil, err
		}
		if strings.TrimSpace(stop.Reason) == "" || len(stop.Reason) > MaxMessageBytes {
			return nil, errors.New("invalid probe stop reason")
		}
		if err := validRedactedMessage(stop.Reason); err != nil {
			return nil, err
		}
		return stop, nil
	case CommandHistoryClear:
		if target.AssignmentGeneration == nil || target.SourceAlertID != nil {
			return nil, errors.New("history.clear requires assignment generation without an incident")
		}
		if err := rejectUnknownKeys(fields, "monitor_id", "assignment_generation", "through_observed_at", "through_seq", "clear_id"); err != nil {
			return nil, err
		}
		var clear HistoryClearData
		if err := decodeRequiredFields(fields,
			field{"monitor_id", &clear.MonitorID}, field{"assignment_generation", &clear.AssignmentGeneration},
			field{"through_observed_at", &clear.ThroughObservedAt}, field{"through_seq", &clear.ThroughSeq},
		); err != nil {
			return nil, err
		}
		if err := requiredUUID(fields, "clear_id", &clear.ClearID); err != nil {
			return nil, err
		}
		if clear.MonitorID <= 0 || clear.AssignmentGeneration <= 0 || clear.ThroughSeq <= 0 {
			return nil, errors.New("history.clear requires positive monitor, generation, and sequence bounds")
		}
		if clear.AssignmentGeneration != *target.AssignmentGeneration {
			return nil, errors.New("history.clear target and data generation disagree")
		}
		return clear, nil
	case CommandCredentialPrepare:
		if err := requireProbeWideTarget(target); err != nil {
			return nil, err
		}
		if err := rejectUnknownKeys(fields, "rotation_id", "credential_version", "token", "overlap_expires_at"); err != nil {
			return nil, err
		}
		var prepare CredentialPrepareData
		if err := requiredUUID(fields, "rotation_id", &prepare.RotationID); err != nil {
			return nil, err
		}
		if err := decodeRequiredFields(fields,
			field{"credential_version", &prepare.CredentialVersion}, field{"token", &prepare.Token},
			field{"overlap_expires_at", &prepare.OverlapExpiresAt},
		); err != nil {
			return nil, err
		}
		if prepare.CredentialVersion <= 0 || !validRuntimeToken(prepare.Token) {
			return nil, errors.New("credential.prepare requires a positive version and runtime token")
		}
		return prepare, nil
	case CommandCredentialActivate:
		if err := requireProbeWideTarget(target); err != nil {
			return nil, err
		}
		if err := rejectUnknownKeys(fields, "rotation_id", "credential_version"); err != nil {
			return nil, err
		}
		var activate CredentialActivateData
		if err := requiredUUID(fields, "rotation_id", &activate.RotationID); err != nil {
			return nil, err
		}
		if err := required(fields, "credential_version", &activate.CredentialVersion); err != nil {
			return nil, err
		}
		if activate.CredentialVersion <= 0 {
			return nil, errors.New("credential.activate requires a positive version")
		}
		return activate, nil
	case CommandCertificatePrepare:
		if err := requireProbeWideTarget(target); err != nil {
			return nil, err
		}
		if err := rejectUnknownKeys(fields, "rotation_id", "certificate_version", "valid_for_days"); err != nil {
			return nil, err
		}
		var prepare CertificatePrepareData
		if err := requiredUUID(fields, "rotation_id", &prepare.RotationID); err != nil {
			return nil, err
		}
		if err := decodeRequiredFields(fields,
			field{"certificate_version", &prepare.CertificateVersion}, field{"valid_for_days", &prepare.ValidForDays},
		); err != nil {
			return nil, err
		}
		if prepare.CertificateVersion <= 0 || prepare.ValidForDays < 1 || prepare.ValidForDays > MaxCertificateValidDays {
			return nil, errors.New("certificate.prepare requires a positive version and bounded lifetime")
		}
		return prepare, nil
	case CommandCertificateActivate:
		if err := requireProbeWideTarget(target); err != nil {
			return nil, err
		}
		if err := rejectUnknownKeys(fields, "rotation_id", "certificate_version", "expected_fingerprint"); err != nil {
			return nil, err
		}
		var activate CertificateActivateData
		if err := requiredUUID(fields, "rotation_id", &activate.RotationID); err != nil {
			return nil, err
		}
		if err := decodeRequiredFields(fields,
			field{"certificate_version", &activate.CertificateVersion}, field{"expected_fingerprint", &activate.ExpectedFingerprint},
		); err != nil {
			return nil, err
		}
		if activate.CertificateVersion <= 0 || !validSHA256(activate.ExpectedFingerprint) {
			return nil, errors.New("certificate.activate requires a positive version and TLS fingerprint")
		}
		return activate, nil
	default:
		return nil, fmt.Errorf("unsupported command kind: %w", ErrUnsupportedPayload)
	}
}

func requireProbeWideTarget(target CommandTarget) error {
	if target.AssignmentGeneration != nil || target.SourceAlertID != nil {
		return errors.New("credential and certificate commands cannot target an assignment or incident")
	}
	return nil
}

func decodeCommandResultDetails(data []byte) (CommandResultDetails, error) {
	if err := rejectSecretPayload(data); err != nil {
		return nil, err
	}
	fields, err := objectFields(data)
	if err != nil {
		return nil, err
	}
	switch {
	case hasExactKeys(fields, "credential_version"):
		var details CredentialPrepareDetails
		if err := required(fields, "credential_version", &details.CredentialVersion); err != nil {
			return nil, err
		}
		if details.CredentialVersion <= 0 {
			return nil, errors.New("credential prepare details require a positive version")
		}
		return details, nil
	case hasExactKeys(fields, "certificate_version", "tls_fingerprint", "not_after"):
		var details CertificatePrepareDetails
		if err := decodeRequiredFields(fields,
			field{"certificate_version", &details.CertificateVersion}, field{"tls_fingerprint", &details.TLSFingerprint},
			field{"not_after", &details.NotAfter},
		); err != nil {
			return nil, err
		}
		if details.CertificateVersion <= 0 || !validSHA256(details.TLSFingerprint) {
			return nil, errors.New("certificate prepare details require a positive version and TLS fingerprint")
		}
		return details, nil
	default:
		return nil, errors.New("unsupported command result details")
	}
}

func hasExactKeys(fields map[string]json.RawMessage, keys ...string) bool {
	if len(fields) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return false
		}
	}
	return true
}

func validRedactedMessage(value string) error {
	if len(value) > MaxMessageBytes {
		return errors.New("message exceeds maximum bytes")
	}
	if looksLikeSecret(value) {
		return errors.New("message must not contain a token or private key")
	}
	return nil
}

func looksLikeSecret(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(value, "phx_probe_") || strings.Contains(value, "BEGIN ") || strings.Contains(lower, "private_key")
}

func rejectSecretPayload(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decoding secret scan: %w", err)
	}
	return walkSecrets(value)
}

func walkSecrets(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if key == "token" || key == "private_key" || key == "enrollment_token" {
				return errors.New("result must not contain a token or private key")
			}
			if err := walkSecrets(nested); err != nil {
				return err
			}
		}
	case []any:
		for _, nested := range typed {
			if err := walkSecrets(nested); err != nil {
				return err
			}
		}
	case string:
		if looksLikeSecret(typed) {
			return errors.New("result must not contain a token or private key")
		}
	}
	return nil
}
