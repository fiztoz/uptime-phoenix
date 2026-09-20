package domain

import "time"

// LocalHeartbeatCommit is an evaluated local result based on ExpectedStateSeq.
// Zero expects no regional state. The repository assigns Heartbeat.ID and
// SourceSeq; callers supply the active assignment generation and UTC check time.
type LocalHeartbeatCommit struct {
	Heartbeat        Heartbeat
	RawStatus        Status
	ExpectedStateSeq int64
	Incident         *RegionalIncident
	Alert            *Alert
	DeliveryIntents  []DeliveryIntent
	Escalation       *AlertEscalation
	ThrottleUpdate   bool
	ThrottleClear    bool
	LegacyConfig     bool          // Ordinary local runtime has no applied execution contract.
	ResendInterval   time.Duration // Positive for a still-DOWN resend, reserved atomically.
}
