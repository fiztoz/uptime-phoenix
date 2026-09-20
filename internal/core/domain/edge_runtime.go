package domain

import "time"

// EdgeIdentity binds the protected local files to one durable telemetry stream.
// HubID is empty until a single-use enrollment is durably accepted.
type EdgeIdentity struct {
	ProbeID              string
	StreamID             string
	Fingerprint          string
	HubID                string
	LastCreatedSeq       int64
	CommittedSeq         int64
	ConnectionGeneration int64
	ConfigRevision       int64
}

// ValidEdgeIdentity checks local identity without permitting reserved hub IDs.
func ValidEdgeIdentity(i EdgeIdentity) bool {
	return configUUID(i.ProbeID) && configUUID(i.StreamID) && len(i.Fingerprint) == 64 && configHex(i.Fingerprint) &&
		(i.HubID == "" || configUUID(i.HubID)) && i.LastCreatedSeq >= 0 && i.CommittedSeq >= 0 &&
		i.CommittedSeq <= i.LastCreatedSeq && i.ConnectionGeneration >= 0 && i.ConfigRevision >= 0
}

// EdgeEnrollment binds an inbound runtime credential after local authorization.
// Only token digests cross this persistence boundary; plaintext is never stored.
type EdgeEnrollment struct {
	HubID             string
	ProbeID           string
	EnrollmentID      string
	CredentialVersion int64
	TokenHash         [32]byte
	AppliedAt         time.Time
	// ValidUntil bounds an authenticated overlap session; nil means current.
	// It is supplied by storage and must be rechecked at socket admission.
	ValidUntil *time.Time
	// Certificate identity is supplied by the server's actual TLS handshake, not
	// by a remote header. Atomic admission rechecks its current overlap eligibility.
	CertificateFingerprint string
	CertificateNotBefore   time.Time
	CertificateNotAfter    time.Time
}

// ValidEdgeEnrollment checks immutable binding and digest metadata.
func ValidEdgeEnrollment(e EdgeEnrollment) bool {
	return configUUID(e.HubID) && configUUID(e.ProbeID) && configUUID(e.EnrollmentID) &&
		e.CredentialVersion > 0 && e.TokenHash != [32]byte{} && !e.AppliedAt.IsZero()
}

// EdgeEnrollmentToken is a bounded, locally issued, single-use authorization.
type EdgeEnrollmentToken struct {
	Hash      [32]byte
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// EdgeAssignmentIdentity is the nonsecret scheduler index into encrypted config.
type EdgeAssignmentIdentity struct {
	MonitorID  int64
	Generation int64
	Active     bool
}

// EdgeActiveConfig atomically selects exact protected bytes and assignment IDs.
// Runtime settings are decrypted from Snapshot; no plaintext duplicate is stored.
type EdgeActiveConfig struct {
	Snapshot    ProtectedProbeConfig
	Assignments []EdgeAssignmentIdentity
	AppliedAt   time.Time
	// ConnectionGeneration is an input fence checked during activation. Stored
	// active content remains valid when a later authenticated session takes over.
	ConnectionGeneration int64
}

// EdgeResolvedAssignment is a complete confidential execution input. Proxy is
// resolved directly; edge execution never consults mutable hub rows.
type EdgeResolvedAssignment struct {
	Monitor            *Monitor
	Generation         int64
	EffectiveOwner     string
	Tags               []ProbeConfigTag
	NotificationLinks  []MonitorNotification
	MaintenanceIDs     []int64
	EscalationPolicyID *int64
	Proxy              *Proxy
}

// EdgeResolvedChannel binds provider settings to their accepted version.
type EdgeResolvedChannel struct {
	Notification *Notification
	Version      int64
}

// EdgeResolvedConfig is decrypted, validated runtime input. It must never be
// logged, marshaled as a domain object or retained as plaintext in SQLite.
type EdgeResolvedConfig struct {
	Probe       ProbeDisplay
	Watchdog    ProbeWatchdogSettings
	Metadata    ProbeConfigMetadata
	Assignments []EdgeResolvedAssignment
	Channels    map[int64]EdgeResolvedChannel
	Templates   map[int64]*NotificationTemplate
	Maintenance map[int64]*MaintenanceWindow
	Policies    map[int64]*EscalationPolicy
}

// EdgeMonitorEvidence is source state for one immutable assignment generation.
// LastEnqueuedAt throttles intent creation, independently of provider completion.
type EdgeMonitorEvidence struct {
	State          *RegionalState
	Incident       *RegionalIncident
	LastEnqueuedAt *time.Time
}

// EdgeCheckRecord commits evaluated source evidence and optional lifecycle/work.
// Sequence numbers are allocated by the store inside the same transaction.
type EdgeCheckRecord struct {
	ExpectedStateSeq int64
	// ACK changes incident state without inventing an observation sequence.
	ExpectedIncidentVersion int64
	Observation             RegionalObservation
	Incident                *RegionalIncident
	DeliveryIntents         []DeliveryIntent
}

// EdgeTelemetryRecord retains exact bounded event bytes for later hub replay.
type EdgeTelemetryRecord struct {
	Seq        int64
	Kind       string
	ObservedAt time.Time
	Payload    []byte
}
