package domain

import "time"

// LocalProbeID is the immutable registration ID and key of the existing scheduler.
const LocalProbeID = "local"

// LocalStreamID is the hub-owned telemetry stream for local scheduler results.
const LocalStreamID = "00000000-0000-4000-8000-000000000001"

// NormalizeProbeID returns the reserved local identity when the caller omitted one.
func NormalizeProbeID(id string) string {
	if id == "" {
		return LocalProbeID
	}
	return id
}

// ProbeKind identifies the location of a logical execution vantage point.
type ProbeKind string

const (
	// ProbeKindLocal identifies the installation's existing scheduler.
	ProbeKindLocal ProbeKind = "local"
	// ProbeKindRemote identifies a registered external vantage point.
	ProbeKindRemote ProbeKind = "remote"
)

// Probe is registration metadata only. It grants no execution or authentication
// authority and deliberately contains no credentials or transient runtime state.
// ID, Key, and Kind are immutable after creation. The local row is immutable.
//
// Endpoint and TLSPin are the operator-declared network trust of the remote
// vantage point (frozen home: the registration itself). They are set at
// creation and immutable afterward — changing them is an explicit identity
// workflow, never an ordinary update. Enrollment copies them into the prepared
// connection; neither value is authentication material.
type Probe struct {
	RevokedAt *time.Time
	DeletedAt *time.Time
	ID        string
	Key       string
	Name      string
	Location  string
	Kind      ProbeKind
	Enabled   bool
	Endpoint  string
	TLSPin    string
	Revision  int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ProbeAssignment identifies one active monitor/vantage-point relationship.
// Generation begins at one and increases when a removed assignment is recreated.
// Removing an assignment retains its generation in a repository tombstone.
type ProbeAssignment struct {
	ResourceBinding *ProbeResourceBinding
	MonitorID       int64
	ProbeID         string
	Generation      int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// MonitorProbeAssignments is one atomically replaceable desired assignment set.
// Assignments contains active members only, ordered by ProbeID. Revision starts
// at one and changes when the member set, bindings, HealthPolicy, or
// AlertDelivery changes. An empty AlertDelivery is the legacy regional mode.
type MonitorProbeAssignments struct {
	MonitorID     int64
	Revision      int64
	HealthPolicy  HealthPolicy
	AlertDelivery AlertDelivery
	Assignments   []ProbeAssignment
}

// LocalWorkerMayRun reports whether the hub scheduler may execute this set.
// A nil set is legacy (no assignment row yet) and remains locally runnable.
func LocalWorkerMayRun(set *MonitorProbeAssignments) bool {
	if set == nil {
		return true
	}
	for _, assignment := range set.Assignments {
		if assignment.ProbeID == LocalProbeID {
			return true
		}
	}
	return false
}
