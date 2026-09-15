package probe

import (
	"encoding/json"
	"errors"
	"strings"
)

// RotateCredentialRequest is the HTTP body for POST .../rotate-credential.
type RotateCredentialRequest struct {
	CredentialVersion Decimal `json:"credential_version"`
}

// RevokeRequest is the HTTP body for POST .../revoke.
type RevokeRequest struct {
	Reason string `json:"reason"`
}

// ResetStreamRequest is the HTTP body for POST .../reset-stream.
type ResetStreamRequest struct {
	StreamID              string `json:"stream_id"`
	EnrollmentOperationID string `json:"enrollment_operation_id"`
}

// OperationError is a redacted failed-operation diagnostic.
type OperationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// OperationReceipt is an admin operation view. It is not evidence that work ran.
type OperationReceipt struct {
	OperationID string          `json:"operation_id"`
	ProbeID     string          `json:"probe_id"`
	Status      string          `json:"status"`
	Phase       string          `json:"phase"`
	CreatedAt   Timestamp       `json:"created_at"`
	UpdatedAt   Timestamp       `json:"updated_at"`
	Error       *OperationError `json:"error"`
}

// CommandReceipt is the hub-side command view returned before remote confirmation.
type CommandReceipt struct {
	CommandID       string `json:"command_id"`
	Status          string `json:"status"`
	RemoteConfirmed bool   `json:"remote_confirmed"`
}

// RevokeReceipt is an operation receipt plus whether the probe confirmed revocation.
type RevokeReceipt struct {
	OperationReceipt
	RemoteConfirmed bool `json:"remote_confirmed"`
}

// DecodeRotateCredentialRequest validates the rotate-credential HTTP body.
func DecodeRotateCredentialRequest(data []byte) (RotateCredentialRequest, error) {
	var request RotateCredentialRequest
	fields, err := decodeJSONObject(data)
	if err != nil {
		return RotateCredentialRequest{}, err
	}
	if err := rejectUnknownKeys(fields, "credential_version"); err != nil {
		return RotateCredentialRequest{}, err
	}
	if err := required(fields, "credential_version", &request.CredentialVersion); err != nil {
		return RotateCredentialRequest{}, err
	}
	if request.CredentialVersion <= 0 {
		return RotateCredentialRequest{}, errors.New("credential_version must be positive")
	}
	return request, nil
}

// DecodeRevokeRequest validates the revoke HTTP body.
func DecodeRevokeRequest(data []byte) (RevokeRequest, error) {
	var request RevokeRequest
	fields, err := decodeJSONObject(data)
	if err != nil {
		return RevokeRequest{}, err
	}
	if err := rejectUnknownKeys(fields, "reason"); err != nil {
		return RevokeRequest{}, err
	}
	if err := required(fields, "reason", &request.Reason); err != nil {
		return RevokeRequest{}, err
	}
	if strings.TrimSpace(request.Reason) == "" || validRedactedMessage(request.Reason) != nil {
		return RevokeRequest{}, errors.New("invalid revoke reason")
	}
	return request, nil
}

// DecodeResetStreamRequest validates the reset-stream HTTP body.
func DecodeResetStreamRequest(data []byte) (ResetStreamRequest, error) {
	var request ResetStreamRequest
	fields, err := decodeJSONObject(data)
	if err != nil {
		return ResetStreamRequest{}, err
	}
	if err := rejectUnknownKeys(fields, "stream_id", "enrollment_operation_id"); err != nil {
		return ResetStreamRequest{}, err
	}
	if err := requiredUUID(fields, "stream_id", &request.StreamID); err != nil {
		return ResetStreamRequest{}, err
	}
	if err := requiredUUID(fields, "enrollment_operation_id", &request.EnrollmentOperationID); err != nil {
		return ResetStreamRequest{}, err
	}
	return request, nil
}

// DecodeOperationReceipt validates an admin operation receipt shape.
func DecodeOperationReceipt(data []byte) (OperationReceipt, error) {
	receipt, err := decodeOperationReceipt(data, false)
	if err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

// DecodeRevokeReceipt validates a revoke receipt, including remote_confirmed.
func DecodeRevokeReceipt(data []byte) (RevokeReceipt, error) {
	receipt, err := decodeOperationReceipt(data, true)
	if err != nil {
		return RevokeReceipt{}, err
	}
	fields, err := decodeJSONObject(data)
	if err != nil {
		return RevokeReceipt{}, err
	}
	var confirmed bool
	if err := required(fields, "remote_confirmed", &confirmed); err != nil {
		return RevokeReceipt{}, err
	}
	return RevokeReceipt{OperationReceipt: receipt, RemoteConfirmed: confirmed}, nil
}

// DecodeCommandReceipt validates a hub command receipt. It is not a remote apply.
func DecodeCommandReceipt(data []byte) (CommandReceipt, error) {
	var receipt CommandReceipt
	fields, err := decodeJSONObject(data)
	if err != nil {
		return CommandReceipt{}, err
	}
	if err := rejectSecretPayload(data, nil); err != nil {
		return CommandReceipt{}, err
	}
	if err := rejectUnknownKeys(fields, "command_id", "status", "remote_confirmed"); err != nil {
		return CommandReceipt{}, err
	}
	if err := requiredUUID(fields, "command_id", &receipt.CommandID); err != nil {
		return CommandReceipt{}, err
	}
	if err := decodeRequiredFields(fields, field{"status", &receipt.Status}, field{"remote_confirmed", &receipt.RemoteConfirmed}); err != nil {
		return CommandReceipt{}, err
	}
	switch receipt.Status {
	case "pending", "applied", "failed", "expired":
	default:
		return CommandReceipt{}, errors.New("invalid command receipt status")
	}
	return receipt, nil
}

func decodeOperationReceipt(data []byte, revoke bool) (OperationReceipt, error) {
	var receipt OperationReceipt
	if err := rejectSecretPayload(data, nil); err != nil {
		return OperationReceipt{}, err
	}
	fields, err := decodeJSONObject(data)
	if err != nil {
		return OperationReceipt{}, err
	}
	allowed := []string{"operation_id", "probe_id", "status", "phase", "created_at", "updated_at", "error"}
	if revoke {
		allowed = append(allowed, "remote_confirmed")
	}
	if err := rejectUnknownKeys(fields, allowed...); err != nil {
		return OperationReceipt{}, err
	}
	if err := requiredUUID(fields, "operation_id", &receipt.OperationID); err != nil {
		return OperationReceipt{}, err
	}
	if err := requiredUUID(fields, "probe_id", &receipt.ProbeID); err != nil {
		return OperationReceipt{}, err
	}
	if err := decodeRequiredFields(fields,
		field{"status", &receipt.Status}, field{"phase", &receipt.Phase},
		field{"created_at", &receipt.CreatedAt}, field{"updated_at", &receipt.UpdatedAt},
	); err != nil {
		return OperationReceipt{}, err
	}
	if !validErrorCode(receipt.Phase) {
		return OperationReceipt{}, errors.New("invalid operation phase")
	}
	var errorRaw *json.RawMessage
	if err := nullable(fields, "error", &errorRaw); err != nil {
		return OperationReceipt{}, err
	}
	switch receipt.Status {
	case "pending", "running", "succeeded":
		if errorRaw != nil {
			return OperationReceipt{}, errors.New("non-failed operation receipt requires null error")
		}
	case "failed":
		if errorRaw == nil {
			return OperationReceipt{}, errors.New("failed operation receipt requires an error")
		}
		detail, err := decodeOperationError(*errorRaw)
		if err != nil {
			return OperationReceipt{}, err
		}
		receipt.Error = &detail
	default:
		return OperationReceipt{}, errors.New("invalid operation receipt status")
	}
	return receipt, nil
}

func decodeOperationError(data []byte) (OperationError, error) {
	var detail OperationError
	fields, err := objectFields(data)
	if err != nil {
		return OperationError{}, err
	}
	if err := rejectUnknownKeys(fields, "code", "message"); err != nil {
		return OperationError{}, err
	}
	if err := decodeRequiredFields(fields, field{"code", &detail.Code}, field{"message", &detail.Message}); err != nil {
		return OperationError{}, err
	}
	if !validErrorCode(detail.Code) || validRedactedMessage(detail.Message) != nil {
		return OperationError{}, errors.New("invalid operation error")
	}
	return detail, nil
}
