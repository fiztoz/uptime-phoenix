package probe

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

const (
	// EnrollmentTokenPrefix is the write-only enrollment bearer prefix.
	EnrollmentTokenPrefix = "phx_probe_enroll_"
	// RuntimeTokenPrefix is the write-only runtime credential prefix.
	RuntimeTokenPrefix   = "phx_probe_"
	minTokenEntropyBytes = 32
)

// EnrollRequest offers a persisted runtime credential on the enrollment socket.
// The enrollment token authenticates the WebSocket and is not a JSON member.
type EnrollRequest struct {
	HubID             string   `json:"hub_id"`
	ProbeID           string   `json:"probe_id"`
	EnrollmentID      string   `json:"enrollment_id"`
	ProtocolMin       int32    `json:"protocol_min"`
	ProtocolMax       int32    `json:"protocol_max"`
	Capabilities      []string `json:"capabilities"`
	CredentialVersion Decimal  `json:"credential_version"`
	Token             string   `json:"token"`
}

// EnrollResult is a probe receipt. It does not mark hub enrollment active.
type EnrollResult struct {
	HubID               string     `json:"hub_id"`
	ProbeID             string     `json:"probe_id"`
	EnrollmentID        string     `json:"enrollment_id"`
	Status              string     `json:"status"`
	CredentialVersion   Decimal    `json:"credential_version"`
	AppliedAt           *Timestamp `json:"applied_at"`
	TLSFingerprint      *string    `json:"tls_fingerprint"`
	CertificateNotAfter *Timestamp `json:"certificate_not_after"`
	Code                *string    `json:"code"`
	Message             string     `json:"message"`
}

// EnrollmentTokenRequest is the write-only HTTP enroll body.
type EnrollmentTokenRequest struct {
	EnrollmentToken string `json:"enrollment_token"`
}

// DecodeEnrollRequest validates an enroll.request frame. It does not bind a hub.
func DecodeEnrollRequest(data []byte) (Envelope, EnrollRequest, error) {
	var request EnrollRequest
	envelope, fields, err := telemetryFields(data, "enroll.request")
	if err != nil {
		return envelope, EnrollRequest{}, err
	}
	if err := decodeEnrollmentIdentity(fields, &request.HubID, &request.ProbeID, &request.EnrollmentID); err != nil {
		return envelope, EnrollRequest{}, err
	}
	if err := decodeRequiredFields(fields,
		field{"protocol_min", &request.ProtocolMin}, field{"protocol_max", &request.ProtocolMax},
		field{"capabilities", &request.Capabilities}, field{"credential_version", &request.CredentialVersion},
		field{"token", &request.Token},
	); err != nil {
		return envelope, EnrollRequest{}, err
	}
	if request.ProtocolMin <= 0 || request.ProtocolMax < request.ProtocolMin || request.ProtocolMin > 1 || request.ProtocolMax < 1 {
		return envelope, EnrollRequest{}, errors.New("enrollment protocol range must include V1")
	}
	if request.CredentialVersion <= 0 || !validRuntimeToken(request.Token) {
		return envelope, EnrollRequest{}, errors.New("enrollment requires a positive credential version and runtime token")
	}
	if err := validateCapabilities(request.Capabilities); err != nil {
		return envelope, EnrollRequest{}, err
	}
	if !containsCapability(request.Capabilities, "snapshot.v1") {
		return envelope, EnrollRequest{}, ErrUnsupportedCapability
	}
	return envelope, request, nil
}

// DecodeEnrollResult validates an enroll.result frame without activating enrollment.
func DecodeEnrollResult(data []byte) (Envelope, EnrollResult, error) {
	var result EnrollResult
	envelope, fields, err := telemetryFields(data, "enroll.result")
	if err != nil {
		return envelope, EnrollResult{}, err
	}
	if err := rejectSecretPayload(envelope.Payload, nil); err != nil {
		return envelope, EnrollResult{}, err
	}
	if err := decodeEnrollmentIdentity(fields, &result.HubID, &result.ProbeID, &result.EnrollmentID); err != nil {
		return envelope, EnrollResult{}, err
	}
	if err := decodeRequiredFields(fields,
		field{"status", &result.Status}, field{"credential_version", &result.CredentialVersion},
		field{"message", &result.Message},
	); err != nil {
		return envelope, EnrollResult{}, err
	}
	if err := nullable(fields, "applied_at", &result.AppliedAt); err != nil {
		return envelope, EnrollResult{}, err
	}
	if err := nullable(fields, "tls_fingerprint", &result.TLSFingerprint); err != nil {
		return envelope, EnrollResult{}, err
	}
	if err := nullable(fields, "certificate_not_after", &result.CertificateNotAfter); err != nil {
		return envelope, EnrollResult{}, err
	}
	if err := nullable(fields, "code", &result.Code); err != nil {
		return envelope, EnrollResult{}, err
	}
	if err := validRedactedMessage(result.Message); err != nil {
		return envelope, EnrollResult{}, err
	}
	if result.Code != nil && !validErrorCode(*result.Code) {
		return envelope, EnrollResult{}, errors.New("invalid enrollment result code")
	}
	switch result.Status {
	case "applied", "already_applied":
		if result.CredentialVersion <= 0 || result.AppliedAt == nil || result.TLSFingerprint == nil || result.CertificateNotAfter == nil || result.Code != nil {
			return envelope, EnrollResult{}, errors.New("successful enrollment requires version, applied_at, pin, expiry, and null code")
		}
		if !validSHA256(*result.TLSFingerprint) {
			return envelope, EnrollResult{}, errors.New("invalid enrollment TLS fingerprint")
		}
	case "rejected":
		if result.CredentialVersion != 0 || result.AppliedAt != nil || result.TLSFingerprint != nil || result.CertificateNotAfter != nil || result.Code == nil {
			return envelope, EnrollResult{}, errors.New("rejected enrollment requires code, version zero, and null identity fields")
		}
	case "expired":
		if result.CredentialVersion != 0 || result.AppliedAt != nil || result.TLSFingerprint != nil || result.CertificateNotAfter != nil {
			return envelope, EnrollResult{}, errors.New("expired enrollment requires version zero and null identity fields")
		}
	default:
		return envelope, EnrollResult{}, errors.New("invalid enrollment result status")
	}
	return envelope, result, nil
}

// DecodeEnrollmentTokenRequest validates the write-only HTTP enroll body.
func DecodeEnrollmentTokenRequest(data []byte) (EnrollmentTokenRequest, error) {
	var request EnrollmentTokenRequest
	fields, err := decodeJSONObject(data)
	if err != nil {
		return EnrollmentTokenRequest{}, err
	}
	if err := rejectUnknownKeys(fields, "enrollment_token"); err != nil {
		return EnrollmentTokenRequest{}, err
	}
	if err := required(fields, "enrollment_token", &request.EnrollmentToken); err != nil {
		return EnrollmentTokenRequest{}, err
	}
	if !validEnrollmentToken(request.EnrollmentToken) {
		return EnrollmentTokenRequest{}, errors.New("enrollment_token must use the phx_probe_enroll_ prefix")
	}
	return request, nil
}

func validRuntimeToken(value string) bool {
	if strings.HasPrefix(value, EnrollmentTokenPrefix) {
		return false
	}
	return validPrefixedToken(value, RuntimeTokenPrefix)
}

func validEnrollmentToken(value string) bool {
	return validPrefixedToken(value, EnrollmentTokenPrefix)
}

func validPrefixedToken(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	rest := value[len(prefix):]
	if len(rest) < base64.RawURLEncoding.EncodedLen(minTokenEntropyBytes) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil && len(decoded) >= minTokenEntropyBytes
}

func decodeEnrollmentIdentity(fields map[string]json.RawMessage, hubID, probeID, enrollmentID *string) error {
	if err := requiredUUID(fields, "hub_id", hubID); err != nil {
		return err
	}
	if err := requiredUUID(fields, "probe_id", probeID); err != nil {
		return err
	}
	return requiredUUID(fields, "enrollment_id", enrollmentID)
}
