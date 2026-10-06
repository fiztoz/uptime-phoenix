package domain

import "time"

// LeaseFence pins a queued check to the worker lease instance that authorized
// it. A lease instance is created whenever a worker acquires a monitor lease
// (fresh claim, takeover, or reacquisition after expiry) and stays stable
// across renewals, so an in-flight check keeps its authority until the lease
// expires or another instance replaces it. Recording must validate owner,
// epoch, and expiry atomically inside its transaction before any
// result-derived write. A nil fence means the caller runs without worker
// authority (ordinary local and push recording) and is deliberately not
// fenced.
type LeaseFence struct {
	WorkerID   string        // lease owner recorded at claim time
	LeaseEpoch int64         // lease instance; changes only when a new instance is created
	LeaseTTL   time.Duration // validity window used to re-evaluate expiry at commit time
}

// LeasedMonitor pairs a monitor with the worker lease instance that currently
// owns it. Lease readers return these so a scheduler can capture a LeaseFence
// when queuing work instead of trusting an in-memory snapshot later.
type LeasedMonitor struct {
	Monitor    *Monitor
	WorkerID   string
	LeaseEpoch int64
	LeasedAt   time.Time
}

// LocalHeartbeatCommit is an evaluated local result based on ExpectedStateSeq.
// Zero expects no regional state. The repository assigns Heartbeat.ID and
// SourceSeq; callers supply the active assignment generation and UTC check time.
type LocalHeartbeatCommit struct {
	Heartbeat        Heartbeat
	RawStatus        Status
	ExpectedStateSeq int64
	// LeaseFence, when non-nil, must still authorize this commit inside the
	// recording transaction or the whole commit is rejected with
	// ports.ErrStaleLease. Nil keeps ordinary local and push recording
	// supported without worker authority.
	LeaseFence      *LeaseFence
	Incident        *RegionalIncident
	Alert           *Alert
	DeliveryIntents []DeliveryIntent
	Escalation      *AlertEscalation
	ThrottleUpdate  bool
	ThrottleClear   bool
	LegacyConfig    bool          // Ordinary local runtime has no applied execution contract.
	ResendInterval  time.Duration // Positive for a still-DOWN resend, reserved atomically.
}
