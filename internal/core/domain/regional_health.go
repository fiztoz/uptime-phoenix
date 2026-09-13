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
