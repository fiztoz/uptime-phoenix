package domain

import (
	"math"
	"time"
)

// EdgeCurrentSnapshot is a coherent read of accepted configuration, stream
// progress and retained current evidence. Event bytes survive historical pruning.
type EdgeCurrentSnapshot struct {
	Identity  EdgeIdentity
	CreatedAt time.Time
	States    []EdgeCurrentEvidence
}

// EdgeCurrentEvidence binds an exact source observation to its active incident.
// A resolved incident is omitted; absence does not manufacture a recovery.
type EdgeCurrentEvidence struct {
	MonitorID            int64
	AssignmentGeneration int64
	Seq                  int64
	Payload              []byte
	ActiveSourceAlertID  *string
}

// ProbeCurrentState carries source-evaluated availability independently of history.
type ProbeCurrentState struct {
	MonitorID            int64
	AssignmentGeneration int64
	Seq                  int64
	ObservedAt           time.Time
	Status               Status
	DownCount            int
	Ping                 int
	Message              string
	ActiveSourceAlertID  *string
}

// ProbeCurrentSnapshot is the complete authenticated current-state candidate.
// SHA256 covers original explicit wire DTO bytes, never a marshaled domain value.
type ProbeCurrentSnapshot struct {
	ProbeID        string
	StreamID       string
	SnapshotID     string
	SHA256         string
	ConfigRevision int64
	CreatedAt      time.Time
	LastCreatedSeq int64
	States         []ProbeCurrentState
}

// ProbeStateReceipt proves current projection durability, not history ingestion.
type ProbeStateReceipt struct {
	SnapshotID     string
	StreamID       string
	ConfigRevision int64
	SHA256         string
	AppliedAt      time.Time
	StateCount     int
}

// ProbeStateAuthorityFacts describes the exact durably applied configuration,
// decoded and checked under the projection transaction's session authority.
type ProbeStateAuthorityFacts struct {
	ProbeID        string
	StreamID       string
	ConfigRevision int64
	Assignments    []EdgeAssignmentIdentity
}

// ValidProbeCurrentSnapshot enforces core identity and bounded availability data.
func ValidProbeCurrentSnapshot(session ProbeReplaySession, s ProbeCurrentSnapshot) bool {
	if !ValidHubID(session.HubID) || !ValidHubID(session.OwnerID) || !ValidHubID(session.ProbeID) || !ValidHubID(session.StreamID) || session.ConnectionGeneration <= 0 || s.ProbeID != session.ProbeID || s.StreamID != session.StreamID || !ValidHubID(s.SnapshotID) || !ValidKeyHash(s.SHA256) || s.ConfigRevision <= 0 || s.CreatedAt.IsZero() || s.LastCreatedSeq < 0 || len(s.States) > 10000 {
		return false
	}
	monitors, sequences := make(map[int64]bool, len(s.States)), make(map[int64]bool, len(s.States))
	for _, state := range s.States {
		if state.MonitorID <= 0 || state.AssignmentGeneration <= 0 || state.Seq <= 0 || state.Seq > s.LastCreatedSeq || state.ObservedAt.IsZero() || state.DownCount < 0 || state.DownCount > math.MaxInt32 || state.Ping < 0 || state.Ping > math.MaxInt32 || len(state.Message) > 4096 || monitors[state.MonitorID] || sequences[state.Seq] {
			return false
		}
		switch state.Status {
		case StatusUp, StatusMaintenance:
			if state.DownCount != 0 {
				return false
			}
		case StatusDown:
			if state.DownCount == 0 {
				return false
			}
		case StatusPending:
		default:
			return false
		}
		if state.ActiveSourceAlertID != nil && !ValidHubID(*state.ActiveSourceAlertID) {
			return false
		}
		monitors[state.MonitorID], sequences[state.Seq] = true, true
	}
	return true
}
