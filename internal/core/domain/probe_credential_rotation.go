package domain

import (
	"fmt"
	"time"
)

// ErrProbeCredentialRejected means a pinned runtime dial received HTTP 401
// before a websocket existed. Other authorization failures cannot enable fallback.
var ErrProbeCredentialRejected = fmt.Errorf("probe runtime credential rejected: %w", ErrUnauthorized)

// ProbeCredentialOverlap is the fixed preparation/activation opportunity. A retry
// never moves this deadline; source receipt recovery may continue after it.
const ProbeCredentialOverlap = 10 * time.Minute

// ProbeCredentialRotationIssue is trusted operator input. Both IDs and the new
// version are explicit so a lost operator response cannot generate a second token.
type ProbeCredentialRotationIssue struct {
	HubID             string
	ProbeID           string
	RotationID        string
	CredentialVersion int64
}

// ProbeCredentialRotation is nonsecret operation metadata. Active means a durable
// source activation result was confirmed, not that a candidate authenticated.
type ProbeCredentialRotation struct {
	RotationID        string
	Candidate         ProbeCredentialMetadata
	PreviousVersion   int64
	PrepareCommandID  string
	ActivateCommandID string
	CreatedAt         time.Time
	OverlapExpiresAt  time.Time
	State             string // preparing, activating, active, failed
	PreparedAt        *time.Time
	ActivatedAt       *time.Time
	FailureCode       string
	UpdatedAt         time.Time
}

// ProtectedProbeCredentialRotation supplies one immutable protected issuance.
// The source token and both command bodies are recoverable only with the hub key.
type ProtectedProbeCredentialRotation struct {
	ProbeCredentialRotation
	ProtectedCredential []byte
	PrepareCommand      ProtectedProbeCommand
	ActivateCommand     ProtectedProbeCommand
}

// ProbeCredentialSelection is confidential input for one fenced connection.
// Candidate is tried first when activation is pending; Current is the only
// permitted fallback, and only after an HTTP authentication rejection.
type ProbeCredentialSelection struct {
	Current    ProbeConnection
	Candidate  *ProbeConnection
	RotationID string
}

// ProbeCommandCapabilities restricts claims to effects the peer can execute.
// An unsupported pending command must not starve other supported commands.
type ProbeCommandCapabilities struct {
	AlertAcknowledgement bool
	CredentialRotation   bool
}

// ValidProbeCredentialCommand validates the closed source effect and its fixed
// precision. Authenticated authority is checked separately inside source storage.
func ValidProbeCredentialCommand(c ProbeCredentialCommand) bool {
	if !configUUID(c.CommandID) || !configUUID(c.ProbeID) || !configUUID(c.RotationID) || c.CredentialVersion <= 0 || c.PayloadHash == [32]byte{} || c.CreatedAt.IsZero() || !c.ExpiresAt.After(c.CreatedAt) || c.ExpiresAt.Sub(c.CreatedAt) > 7*24*time.Hour || c.CreatedAt.Nanosecond()%1000 != 0 || c.ExpiresAt.Nanosecond()%1000 != 0 {
		return false
	}
	switch c.Kind {
	case "credential.prepare":
		return c.TokenHash != [32]byte{} && c.OverlapExpiresAt.After(c.CreatedAt) && !c.OverlapExpiresAt.After(c.CreatedAt.Add(ProbeCredentialOverlap)) && c.OverlapExpiresAt.Nanosecond()%1000 == 0
	case "credential.activate":
		return c.TokenHash == [32]byte{} && c.OverlapExpiresAt.IsZero()
	default:
		return false
	}
}
