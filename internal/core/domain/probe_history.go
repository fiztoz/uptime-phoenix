package domain

import "time"

// RegionalHistoryRollup contains sample statistics and disjoint evidence
// durations for one assigned probe in one UTC bucket. Missing checks contribute
// UNKNOWN duration, never fabricated samples or latency.
type RegionalHistoryRollup struct {
	MonitorID    int64
	ProbeID      string
	Bucket       time.Time
	Resolution   string
	UpCount      int
	DownCount    int
	PendingCount int
	MaintCount   int
	UnknownCount int
	TotalChecks  int
	PingCount    int
	AvgPing      float64
	MinPing      int
	MaxPing      int
	Durations    HealthDurations
}

// ProbeHistoryProjection is the pure output of one coherent closed window.
// Storage persists this result and consumes its dirty marker in one transaction.
type ProbeHistoryProjection struct {
	Regional *RegionalHistoryRollup
	Overall  []MonitorHealthInterval
}

// ProbeHistoryWork binds a dirty identity to transactionally read source evidence.
type ProbeHistoryWork struct {
	Bucket   DirtyBucket
	Evidence OverallHistoryInput
	Children []RegionalHistoryRollup
}
