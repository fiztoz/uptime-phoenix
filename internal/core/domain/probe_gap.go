package domain

import "time"

// ProbeTelemetryGap is durable evidence that a contiguous source range is lost.
// Empty monitor IDs mean unknown coverage, never an unaffected assignment set.
type ProbeTelemetryGap struct {
	StreamID           string
	FromSeq            int64
	ThroughSeq         int64
	Reason             string
	ObservedFrom       time.Time
	ObservedThrough    time.Time
	AffectedMonitorIDs []int64
}
