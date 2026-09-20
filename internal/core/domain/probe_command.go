package domain

import "time"

// MaxProbeCommandBytes bounds one immutable control payload before protection.
const MaxProbeCommandBytes = 64 << 10

// ProbeCommandMetadata is immutable authenticated scope for protected request
// bytes. Timestamps use the hub storage's microsecond precision before sealing.
type ProbeCommandMetadata struct {
	CommandID            string
	HubID                string
	ProbeID              string
	StreamID             string
	Kind                 string
	SourceAlertID        *string
	AssignmentGeneration *int64
	CreatedAt            time.Time
	ExpiresAt            time.Time
	PayloadSHA256        string
}

// ProtectedProbeCommand retains the exact request for retries. Plaintext is
// available only through the protector to trusted command dispatch/authorization.
type ProtectedProbeCommand struct {
	ProbeCommandMetadata
	ProtectedPayload []byte
}

// ValidProbeCommandMetadata checks bounded immutable control scope. It grants no
// permission to issue a command or execute a supported wire kind.
func ValidProbeCommandMetadata(m ProbeCommandMetadata) bool {
	if !configUUID(m.CommandID) || !configUUID(m.HubID) || !configUUID(m.ProbeID) || !configUUID(m.StreamID) || !ValidKeyHash(m.PayloadSHA256) || m.CreatedAt.IsZero() || !m.ExpiresAt.After(m.CreatedAt) || m.ExpiresAt.Sub(m.CreatedAt) > 7*24*time.Hour || m.CreatedAt.Nanosecond()%1000 != 0 || m.ExpiresAt.Nanosecond()%1000 != 0 {
		return false
	}
	switch m.Kind {
	case "alert.ack":
		return m.SourceAlertID != nil && configUUID(*m.SourceAlertID) && m.AssignmentGeneration != nil && *m.AssignmentGeneration > 0
	case "history.clear":
		return m.SourceAlertID == nil && m.AssignmentGeneration != nil && *m.AssignmentGeneration > 0
	case "probe.stop", "credential.prepare", "credential.activate", "certificate.prepare", "certificate.activate":
		return m.SourceAlertID == nil && m.AssignmentGeneration == nil
	default:
		return false
	}
}

// EdgeCommandAuthority is supplied by the authenticated session, never by the
// command payload. A reconnect changes only ConnectionGeneration.
type EdgeCommandAuthority struct {
	HubID                string
	ProbeID              string
	StreamID             string
	ConnectionGeneration int64
}

// ProbeAlertAcknowledgement targets one immutable regional incident. PayloadHash
// is SHA-256 of the exact command.request payload, excluding its session envelope.
// The adapter validates the wire shape before crossing this port boundary.
type ProbeAlertAcknowledgement struct {
	CommandID            string
	ProbeID              string
	SourceAlertID        string
	AssignmentGeneration int64
	CreatedAt            time.Time
	ExpiresAt            time.Time
	ActorDisplayName     string
	Note                 *string
	PayloadHash          [32]byte
}

// ProbeCommandOutcome is a durable, nonsecret source receipt. A duplicate request
// returns the original outcome, including its original AppliedAt timestamp.
type ProbeCommandOutcome struct {
	CommandID string
	Status    string
	AppliedAt *time.Time
	Code      string
	Message   string
	// CredentialVersion is present only for successful credential preparation.
	CredentialVersion int64
}

// ProbeCredentialCommand binds one closed prepare/activate operation. TokenHash
// and OverlapExpiresAt are present only for preparation. Plaintext tokens never
// cross the source persistence boundary.
type ProbeCredentialCommand struct {
	CommandID         string
	ProbeID           string
	Kind              string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	PayloadHash       [32]byte
	RotationID        string
	CredentialVersion int64
	TokenHash         [32]byte
	OverlapExpiresAt  time.Time
}

// ProbeAcknowledgementIssue is trusted operator input. CommandID may be omitted
// to allocate a new UUID; retries with an explicit ID preserve original bytes.
type ProbeAcknowledgementIssue struct {
	CommandID            string
	HubID                string
	ProbeID              string
	SourceAlertID        string
	AssignmentGeneration int64
	ActorDisplayName     string
	Note                 *string
	Lifetime             time.Duration
}

// ProbeCommandDispatch contains confidential exact bytes for one authorized send.
// Never marshal or log this domain value; the adapter wraps Payload unchanged.
type ProbeCommandDispatch struct {
	Metadata ProbeCommandMetadata
	Payload  []byte
}
