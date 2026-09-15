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
}

// RegionalIncident is a source-owned incident identity. Hub mirror IDs are distinct.
type RegionalIncident struct {
	SourceAlertID        string
	Scope                IncidentScope
	MonitorID            int64
	ProbeID              string
	AssignmentGeneration int64
	Status               string
	TransitionVersion    int64
	StartedAt            time.Time
	ResolvedAt           *time.Time
	AckedAt              *time.Time
}

// ProbeCommand is durable command identity. Payload secrets stay in the adapter.
type ProbeCommand struct {
	CommandID            string
	ProbeID              string
	Kind                 string
	SourceAlertID        *string
	AssignmentGeneration *int64
	ExpiresAt            time.Time
	Status               string
	RemoteConfirmed      bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// RegionalCommit is one atomic local/edge recording. Notification I/O is outside.
type RegionalCommit struct {
	Observation RegionalObservation
	State       RegionalState
	Incident    *RegionalIncident
}

// ProbeIngestBatch is one contiguous stream prefix for hub ingest.
type ProbeIngestBatch struct {
	ProbeID    string
	StreamID   string
	FromSeq    int64
	ThroughSeq int64
	Events     []RegionalObservation
}
