package domain

import "time"

// Administrative operation kinds. Only hub-side work that is durably recorded
// may name one of these; there is no catch-all kind.
const (
	ProbeOperationRevoke           = "revoke"
	ProbeOperationEnroll           = "enroll"
	ProbeOperationRotateCredential = "rotate_credential"
	ProbeOperationResetStream      = "reset_stream"
)

// Operation lifecycle statuses. succeeded means the hub-side durable work
// committed; a remote peer may still confirm later (revocation-style receipts
// carry that separately). failed always carries a bounded redacted error.
const (
	ProbeOperationPending   = "pending"
	ProbeOperationRunning   = "running"
	ProbeOperationSucceeded = "succeeded"
	ProbeOperationFailed    = "failed"
)

// ProbeOperationError is a bounded, redacted operation diagnostic. It never
// carries tokens, credentials, configuration or raw error chains.
type ProbeOperationError struct {
	Code    string
	Message string
}

// ProbeOperation is one durable administrative operation receipt. It is the
// only proof that accepted work exists: an HTTP 202 must never precede the
// persisted row. Phase is a bounded machine token (1-128 lowercase letters,
// digits or underscores) naming where the operation is working.
type ProbeOperation struct {
	Error       *ProbeOperationError
	CreatedAt   time.Time
	UpdatedAt   time.Time
	OperationID string
	ProbeID     string
	Kind        string
	Status      string
	Phase       string
}

// ValidProbeOperationID accepts canonical non-nil lowercase UUIDs only. The
// reserved local identity is never an operation target or identity.
func ValidProbeOperationID(id string) bool {
	return configUUID(id) && id != LocalProbeID
}

// ValidProbeOperationPhase enforces the bounded machine-token phase contract.
func ValidProbeOperationPhase(phase string) bool {
	if len(phase) < 1 || len(phase) > 128 {
		return false
	}
	for _, c := range phase {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// ValidProbeOperation enforces the complete stored receipt invariant: canonical
// identities, a known kind and status, a bounded phase, and a redacted error
// present exactly when the operation failed.
func ValidProbeOperation(o ProbeOperation) bool {
	if !ValidProbeOperationID(o.OperationID) || !ValidHubID(o.ProbeID) || o.ProbeID == LocalProbeID ||
		!ValidProbeOperationPhase(o.Phase) || o.CreatedAt.IsZero() || o.UpdatedAt.IsZero() {
		return false
	}
	switch o.Kind {
	case ProbeOperationEnroll, ProbeOperationRotateCredential, ProbeOperationResetStream, ProbeOperationRevoke:
	default:
		return false
	}
	switch o.Status {
	case ProbeOperationPending, ProbeOperationRunning, ProbeOperationSucceeded:
		return o.Error == nil
	case ProbeOperationFailed:
		return o.Error != nil && ValidProbeOperationError(*o.Error)
	default:
		return false
	}
}

// ValidProbeOperationError bounds the redacted diagnostic.
func ValidProbeOperationError(e ProbeOperationError) bool {
	return e.Code != "" && len(e.Code) <= 128 && ValidProbeOperationPhase(e.Code) &&
		e.Message != "" && len(e.Message) <= 255
}
