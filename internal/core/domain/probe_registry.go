package domain

import "time"

// LocalProbeID is the immutable registration ID and key of the existing scheduler.
const LocalProbeID = "local"

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
type Probe struct {
	ID        string
	Key       string
	Name      string
	Location  string
	Kind      ProbeKind
	Enabled   bool
	Revision  int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ProbeAssignment identifies one active monitor/vantage-point relationship.
// Generation begins at one and increases when a removed assignment is recreated.
// Removing an assignment retains its generation in a repository tombstone.
type ProbeAssignment struct {
	MonitorID  int64
	ProbeID    string
	Generation int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// MonitorProbeAssignments is one atomically replaceable desired assignment set.
// Assignments contains active members only, ordered by ProbeID. Revision starts
// at one and changes only when the member set or HealthPolicy changes.
// This foundation is not connected to scheduler or remote-runtime activation.
type MonitorProbeAssignments struct {
	MonitorID    int64
	Revision     int64
	HealthPolicy HealthPolicy
	Assignments  []ProbeAssignment
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
