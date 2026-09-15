package domain

import "time"

// HealthPolicy determines overall availability from independent regional evidence.
type HealthPolicy string

const (
	HealthPolicyAnyDown HealthPolicy = "any_down"
	HealthPolicyAllDown HealthPolicy = "all_down"
)

// RegionalHealthEvidence describes one current assignment, including absent evidence.
// The caller must select the accepted assignment generation and stream first.
// Connection liveness is deliberately absent: only check evidence establishes health.
type RegionalHealthEvidence struct {
	ProbeID       string
	Status        Status
	ObservedAt    time.Time
	FreshFor      time.Duration
	Paused        bool
	UnknownReason string
}

// ProbeHealthCounts partitions the assigned set; paused members never enter quorum.
type ProbeHealthCounts struct {
	Assigned    int
	Up          int
	Down        int
	Pending     int
	Unknown     int
	Maintenance int
	Paused      int
}

// MonitorHealth is a pure projection, not a notification or an HTTP response.
type MonitorHealth struct {
	Status Status
	Reason string
	Counts ProbeHealthCounts
}

// CurrentMonitorHealth is overall health at one instant from complete assignment evidence.
// It is not an HTTP or browser view and does not include historical coverage.
type CurrentMonitorHealth struct {
	MonitorID int64
	Policy    HealthPolicy
	AsOf      time.Time
	Health    MonitorHealth
	Regions   []RegionalHealthEvidence
}

// Health history causes record why an overall interval started.
const (
	HealthHistoryCauseRegional       = "regional"
	HealthHistoryCauseFreshness      = "freshness"
	HealthHistoryCauseAssignment     = "assignment"
	HealthHistoryCausePolicy         = "policy"
	HealthHistoryCauseAdministrative = "administrative"
)

// Dirty-bucket resolutions for late-data recomputation.
const (
	DirtyResolution1m      = "1m"
	DirtyResolution1h      = "1h"
	DirtyResolution1d      = "1d"
	DirtyResolutionOverall = "overall"
)

// MonitorHealthState is the materialized current overall projection for one monitor.
type MonitorHealthState struct {
	MonitorID         int64
	Policy            HealthPolicy
	ProjectionVersion int64
	Status            Status
	Reason            string
	Counts            ProbeHealthCounts
	LastTransitionAt  time.Time
	AsOf              time.Time
}

// AssignmentInterval is one probe's membership during overall reconstruction.
// To is exclusive; the zero value means the assignment remains open.
type AssignmentInterval struct {
	ProbeID    string
	Generation int64
	Policy     HealthPolicy
	Revision   int64
	From       time.Time
	To         time.Time
	Paused     bool
}

// MonitorHealthInterval is one overall availability interval on [From, To).
type MonitorHealthInterval struct {
	From           time.Time
	To             time.Time
	Status         Status
	Reason         string
	Cause          string
	Policy         HealthPolicy
	PolicyRevision int64
	Counts         ProbeHealthCounts
}

// MonitorHealthHistory is reconstructed overall availability for one window.
// Intervals are policy-derived; they are never a union of raw regional samples.
type MonitorHealthHistory struct {
	MonitorID int64
	From      time.Time
	To        time.Time
	Intervals []MonitorHealthInterval
	Durations HealthDurations
}

// OverallHistoryInput is the complete evidence needed to reconstruct overall history.
type OverallHistoryInput struct {
	MonitorID    int64
	From         time.Time
	To           time.Time
	FreshFor     time.Duration
	Paused       bool
	Assignments  []AssignmentInterval
	Observations []RegionalObservation
}

// DirtyBucket is durable late-data recomputation work.
type DirtyBucket struct {
	MonitorID  int64
	ProbeID    string
	Resolution string
	Bucket     time.Time
}

// DirtyBucketsForObservation marks 1m/1h/1d regional rollups and the overall minute.
func DirtyBucketsForObservation(obs RegionalObservation) []DirtyBucket {
	at := obs.ObservedAt.UTC()
	day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	return []DirtyBucket{
		{MonitorID: obs.MonitorID, ProbeID: obs.ProbeID, Resolution: DirtyResolution1m, Bucket: at.Truncate(time.Minute)},
		{MonitorID: obs.MonitorID, ProbeID: obs.ProbeID, Resolution: DirtyResolution1h, Bucket: at.Truncate(time.Hour)},
		{MonitorID: obs.MonitorID, ProbeID: obs.ProbeID, Resolution: DirtyResolution1d, Bucket: day},
		{MonitorID: obs.MonitorID, ProbeID: obs.ProbeID, Resolution: DirtyResolutionOverall, Bucket: at.Truncate(time.Minute)},
	}
}

// HealthDurations contains disjoint policy-derived durations for one history window.
// These durations must never be built by pooling samples from multiple probes.
type HealthDurations struct {
	Up          time.Duration
	Down        time.Duration
	Pending     time.Duration
	Unknown     time.Duration
	Maintenance time.Duration
	Paused      time.Duration
}

// HealthCoverage separates availability over known time from evidence coverage.
// A nil percentage means its denominator is zero, including all-maintenance windows.
type HealthCoverage struct {
	Known           time.Duration
	Unknown         time.Duration
	Maintenance     time.Duration
	UptimePercent   *float64
	CoveragePercent *float64
}

// RetryState belongs to one monitor/probe/assignment generation, never a pooled stream.
type RetryState struct {
	Status    Status
	DownCount int
}

// RetryEvaluation contains the effective status and whether it changed from prior state.
type RetryEvaluation struct {
	State     RetryState
	Important bool
}
