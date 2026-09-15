package domain

import "time"

// Heartbeat represents a single check result for a monitor.
type Heartbeat struct {
	ID        int64
	MonitorID int64
	Status    Status
	Time      time.Time
	Msg       string
	Ping      int // latency in ms (0 if not measured)
	Duration  int // total check duration in ms
	Important bool
	DownCount int // consecutive down count
	// ProbeID is the vantage point that produced this sample. Empty means local.
	ProbeID string
	// StreamID/SourceSeq identify a remote telemetry event. Empty/zero on local rows.
	StreamID  string
	SourceSeq int64
	// AssignmentGeneration is the assignment epoch at observation time.
	AssignmentGeneration int64
	// ReceivedAt is hub ingest time. Zero on unread legacy rows.
	ReceivedAt time.Time
	// ConfigRevision is the accepted snapshot revision at observation time.
	ConfigRevision int64
}
