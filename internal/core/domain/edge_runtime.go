package domain

import (
	"math"
	"time"
)

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

// EdgeCertAlertState is the durable, source-owned certificate paging cursor
// for one immutable assignment generation. It is the edge analog of the hub
// `tls_info` alert columns: AlertThreshold records the most urgent threshold
// already committed for AlertNotAfter, and SourceAlertID points at the single
// open certificate incident for that pair. An empty SourceAlertID always pairs
// with a zero threshold and a nil AlertNotAfter, so a renewed certificate can
// never inherit another certificate's delivered history. Version fences every
// cursor write. The cursor itself never crosses the wire: the hub mirrors source
// incidents and delivery outcomes rather than inferring a source cursor.
type EdgeCertAlertState struct {
	MonitorID            int64
	AssignmentGeneration int64
	ConfigRevision       int64
	AlertNotAfter        *time.Time
	UpdatedAt            time.Time
	SourceAlertID        string
	AlertThreshold       int
	Version              int64
}

// ValidEdgeCertAlertState enforces the cursor's pairing invariant. A threshold
// without the exact certificate expiry it was delivered for is unusable, and a
// dangling open-incident pointer is never allowed.
func ValidEdgeCertAlertState(state *EdgeCertAlertState) bool {
	if state == nil || state.MonitorID <= 0 || state.AssignmentGeneration <= 0 || state.ConfigRevision <= 0 || state.Version < 0 || state.UpdatedAt.IsZero() {
		return false
	}
	switch {
	case state.AlertThreshold == 0:
		return state.AlertNotAfter == nil && state.SourceAlertID == ""
	case state.SourceAlertID == "":
		return false
	default:
		return ValidCertificateSubjectIdentity(int64(state.AlertThreshold), state.AlertNotAfter)
	}
}

// EdgeCertAlertContent is the immutable provider-facing snapshot of one
// committed certificate threshold alert. It travels with the durable delivery
// intent so a retry after a restart renders the same message and never re-derives
// a rounded value from a later clock.
type EdgeCertAlertContent struct {
	NotAfter      time.Time
	Issuer        string
	Message       string
	DaysRemaining int
	Threshold     int
}

// ValidEdgeCertAlertContent requires the exact expiry and bounded text.
func ValidEdgeCertAlertContent(content *EdgeCertAlertContent) bool {
	if content == nil || content.DaysRemaining < 0 || len(content.Message) == 0 || len(content.Message) > 4096 || len(content.Issuer) > 256 {
		return false
	}
	return ValidCertificateSubjectIdentity(int64(content.Threshold), &content.NotAfter)
}

// EdgeMonitorEvidence is source state for one immutable assignment generation.
// LastEnqueuedAt throttles intent creation, independently of provider completion.
type EdgeMonitorEvidence struct {
	State               *RegionalState
	Incident            *RegionalIncident
	LastEnqueuedAt      *time.Time
	Certificate         *EdgeCertAlertState
	CertificateIncident *RegionalIncident
	Conditions          []EdgeConditionState
}

// EdgeConditionState is the durable, source-owned evaluated state of one
// auxiliary condition for one immutable assignment generation. It is the edge
// analog of a regional monitor_conditions row: raw measurement, promotion
// candidate and promoted state, with no delivery cursor. Version fences every
// write so a concurrent recording forces re-evaluation instead of losing or
// duplicating a promotion. DeliveredState is the coarse paging cursor: the
// promoted state the operator has already been told about (empty means none).
// AlertSourceID points at the single open capacity incident for this kind.
type EdgeConditionState struct {
	ConditionEvidence
	AlertSourceID  string
	DeliveredState ConditionState
	Alert          *RegionalIncident
	Version        int64
}

// EdgeConditionWork is one evaluated condition's durable state change plus its
// optional promoted transition and paging lifecycle. The transition is emitted
// as an ordered condition.transition event; it never changes primary
// availability. Remove retires a stored kind whose check was disabled in the
// accepted configuration.
type EdgeConditionWork struct {
	State           ConditionEvidence
	ExpectedVersion int64
	Remove          bool
	Transition      *ConditionTransition
	Alert           *EdgeConditionAlertWork
}

// EdgeConditionAlertWork is the capacity paging lifecycle produced by one
// evaluation: at most one incident transition (open, restate, or resolve), its
// delivery intents with an immutable rendered snapshot, and the cursor to store
// after applying them.
type EdgeConditionAlertWork struct {
	Incident       *RegionalIncident
	Intents        []DeliveryIntent
	Content        *EdgeConditionAlertContent
	OpenAlertID    string
	DeliveredState ConditionState
}

// EdgeConditionAlertContent is the immutable provider-facing snapshot of one
// committed capacity page. It travels with the durable delivery intent so a
// retry after a restart renders the state it was committed for instead of
// re-deriving values from a later clock or a pruned observation.
type EdgeConditionAlertContent struct {
	Kind          string
	State         ConditionState
	PreviousState ConditionState
	Used          *float64
	Limit         *float64
	Percent       *float64
	Threshold     *float64
	Unit          string
	Resource      string
	Scope         string
	Source        string
	Message       string
	ObservedAt    time.Time
}

// ValidEdgeConditionAlertContent requires a renderable, bounded snapshot of one
// confirmed condition page. A page always names the state being reported and
// the prior state the operator knows, which may repeat for a delayed page.
func ValidEdgeConditionAlertContent(content *EdgeConditionAlertContent) bool {
	if content == nil || content.Kind != MonitorConditionSessionPool && content.Kind != MonitorConditionStorage ||
		!content.State.IsValid() || !content.PreviousState.IsValid() || content.ObservedAt.IsZero() ||
		len(content.Message) == 0 || len(content.Message) > 4096 ||
		len(content.Unit) > 256 || len(content.Resource) > 256 || len(content.Scope) > 256 || len(content.Source) > 256 {
		return false
	}
	for _, value := range []*float64{content.Used, content.Limit, content.Percent, content.Threshold} {
		if value != nil && (*value < 0 || *value != *value || *value > math.MaxFloat64) {
			return false
		}
	}
	return true
}

// EdgeCertAlertWork is the certificate paging lifecycle produced by one
// evaluation. Transitions are stored and emitted in order, so an administrative
// retirement of a superseded threshold always precedes the new firing incident,
// and every intent references the final transition. Cursor is the durable state
// to store after applying them.
type EdgeCertAlertWork struct {
	Transitions []RegionalIncident
	Cursor      EdgeCertAlertState
	Certificate *EdgeCertAlertContent
	Intents     []DeliveryIntent
}

// EdgeCheckRecord commits evaluated source evidence and optional lifecycle/work.
// Sequence numbers are allocated by the store inside the same transaction.
type EdgeCheckRecord struct {
	ExpectedStateSeq int64
	// ACK changes incident state without inventing an observation sequence.
	ExpectedIncidentVersion int64
	// ExpectedCertificateVersion fences the certificate cursor the same way the
	// state sequence fences retry state: a concurrent writer that advanced it
	// forces re-evaluation instead of a lost or duplicated threshold alert.
	ExpectedCertificateVersion int64
	Observation                RegionalObservation
	Incident                   *RegionalIncident
	DeliveryIntents            []DeliveryIntent
	Certificate                *EdgeCertAlertWork
	Conditions                 []EdgeConditionWork
}

// EdgeTelemetryRecord retains exact bounded event bytes for later hub replay.
type EdgeTelemetryRecord struct {
	Seq        int64
	Kind       string
	ObservedAt time.Time
	Payload    []byte
}
