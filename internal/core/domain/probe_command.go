package domain

import "time"

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
}
