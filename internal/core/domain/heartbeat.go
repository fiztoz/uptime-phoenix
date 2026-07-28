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
}
