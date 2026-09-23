package domain

import "time"

// AlertDelivery is how regional incidents page. Aggregate paging is optional.
type AlertDelivery string

const (
	AlertDeliveryRegional  AlertDelivery = "regional"
	AlertDeliveryAggregate AlertDelivery = "aggregate"
	AlertDeliveryBoth      AlertDelivery = "both"
)

// IncidentScope distinguishes regional, hub-owned aggregate, and watchdog incidents.
type IncidentScope string

const (
	IncidentScopeRegional        IncidentScope = "regional"
	IncidentScopeAggregate       IncidentScope = "aggregate"
	IncidentScopeProbeConnection IncidentScope = "probe_connection"
)

// ProbeStream is one ordered telemetry epoch for a probe. Sequence is signed 64-bit.
type ProbeStream struct {
	ProbeID      string
	StreamID     string
	CommittedSeq int64
	RetiredAt    *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RegionalObservation is one persisted regional check sample.
// Sequence and assignment generation belong to one stream/assignment, never a pool.
type RegionalObservation struct {
	TLS                  *TLSObservation
	Conditions           []ConditionObservation
	ID                   int64
	MonitorID            int64
	ProbeID              string
	AssignmentGeneration int64
	StreamID             string
	Seq                  int64
	ConfigRevision       int64
	Status               Status
	RawStatus            Status
	DownCount            int
	Ping                 int
	DurationMS           int
	Message              string
	Important            bool
	ObservedAt           time.Time
	ReceivedAt           time.Time
}

// RegionalState is the current assignment evidence used for health projection.
type RegionalState struct {
	TLS                  *TLSObservation
	Conditions           []ConditionEvidence
	MonitorID            int64
	ProbeID              string
	AssignmentGeneration int64
	StreamID             string
	Seq                  int64
	ConfigRevision       int64
	Status               Status
	DownCount            int
	ObservedAt           time.Time
	ReceivedAt           time.Time
	LastSuccessAt        *time.Time
	Ping                 int
	Message              string
	ActiveSourceAlertID  *string
	UnknownReason        string
}

const (
	IncidentSubjectAvailability = "availability"
	IncidentSubjectCapacity     = "capacity"
	IncidentSubjectCertificate  = "certificate"
	IncidentSubjectWatchdog     = "watchdog"
)

const (
	DeliveryStatusSent       = "sent"
	DeliveryStatusRetrying   = "retrying"
	DeliveryStatusFailed     = "failed"
	DeliveryStatusSuperseded = "superseded"
)

const (
	DeliveryEventStatusChange      = "status_change"
	DeliveryEventCertificateExpiry = "certificate_expiry"
	DeliveryEventCapacityCondition = "capacity_condition"
	DeliveryEventProbeConnection   = "probe_connection"
	DeliveryEventIncidentSummary   = "incident_summary"
)

// RegionalIncident is a source-owned incident identity. HubIncidentID is the hub mirror.
// Subject, scope, monitor, generation, and start time are immutable.
type RegionalIncident struct {
	HubIncidentID           int64
	SourceAlertID           string
	Scope                   IncidentScope
	MonitorID               int64
	ProbeID                 string
	AssignmentGeneration    int64
	Status                  string
	TransitionVersion       int64
	StartedAt               time.Time
	ResolvedAt              *time.Time
	AckedAt                 *time.Time
	Reason                  string
	ConfigRevision          int64
	SubjectKind             string
	ConditionKind           string
	CertificateThreshold    int64
	CertificateNotAfter     *time.Time
	AckCommandID            string
	AckActorDisplayName     string
	AckNote                 *string
	EscalationPolicyID      int64
	EscalationPolicyVersion int64
	EscalationStatus        string
	EscalationNextStep      *int64
	EscalationNextRunAt     *time.Time
}

// ValidCertificateIncident enforces the immutable V1 certificate incident
// subject: a regional incident bound to one monitor assignment, one fixed
// threshold and the exact expiry that threshold was crossed for. Capacity and
// escalation identity never belong to a certificate incident.
func ValidCertificateIncident(incident *RegionalIncident) bool {
	if incident == nil || incident.Scope != IncidentScopeRegional || incident.SubjectKind != IncidentSubjectCertificate {
		return false
	}
	if incident.MonitorID <= 0 || incident.AssignmentGeneration <= 0 || incident.ConditionKind != "" {
		return false
	}
	return incident.EscalationPolicyID == 0 && incident.EscalationPolicyVersion == 0 && incident.EscalationStatus == "" &&
		incident.EscalationNextStep == nil && incident.EscalationNextRunAt == nil &&
		ValidCertificateSubjectIdentity(incident.CertificateThreshold, incident.CertificateNotAfter)
}

// ValidCapacityIncident enforces the immutable V1 capacity incident subject: a
// regional incident bound to one monitor assignment and one condition kind. The
// kind survives warning/error changes and recovery; certificate identity and
// escalation never belong to a capacity incident.
func ValidCapacityIncident(incident *RegionalIncident) bool {
	if incident == nil || incident.Scope != IncidentScopeRegional || incident.SubjectKind != IncidentSubjectCapacity {
		return false
	}
	if incident.MonitorID <= 0 || incident.AssignmentGeneration <= 0 ||
		incident.ConditionKind != MonitorConditionSessionPool && incident.ConditionKind != MonitorConditionStorage {
		return false
	}
	return incident.CertificateThreshold == 0 && incident.CertificateNotAfter == nil &&
		incident.EscalationPolicyID == 0 && incident.EscalationPolicyVersion == 0 && incident.EscalationStatus == "" &&
		incident.EscalationNextStep == nil && incident.EscalationNextRunAt == nil
}

// RegionalDelivery is a source outbox outcome. It is not a provider send request.
type RegionalDelivery struct {
	DeliveryID              string
	SourceAlertID           string
	SourceTransitionVersion int64
	ProbeID                 string
	NotificationID          int64
	NotificationVersion     int64
	EventKind               string
	Attempt                 int64
	Status                  string
	ErrorCode               string
	ObservedAt              time.Time
}

// ProbeCommand is the hub's nonsecret durable command status. Pending means the
// source has not confirmed a terminal result, even after the request expires.
type ProbeCommand struct {
	ProbeCommandMetadata
	Status                string
	RemoteConfirmed       bool
	Attempts              int64
	LastAttemptAt         *time.Time
	NextAttemptAt         time.Time
	UpdatedAt             time.Time
	LocalCancellationCode string // Administrative termination; never a source receipt.
	Outcome               *ProbeCommandOutcome
}

// RegionalCommit is one atomic local/edge recording. Notification I/O is outside.
type RegionalCommit struct {
	Observation     RegionalObservation
	State           RegionalState
	Incident        *RegionalIncident
	DeliveryIntents []DeliveryIntent
}

// ProbeIngestBatch is one contiguous stream prefix for hub ingest.
type ProbeIngestBatch struct {
	ProbeID    string
	StreamID   string
	FromSeq    int64
	ThroughSeq int64
	Events     []RegionalObservation
}
