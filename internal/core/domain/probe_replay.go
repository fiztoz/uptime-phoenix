package domain

import (
	"errors"
	"time"
)

// ErrReplayRetry requests bounded backoff after a failed commit with a freshly
// verified durable cursor. The accompanying result never authorizes pruning.
var ErrReplayRetry = errors.New("telemetry storage temporarily unavailable")

// Supported replay event kinds.
const (
	ReplayKindObservation     = "observation"
	ReplayKindAlertTransition = "alert.transition"
	ReplayKindDeliveryResult  = "delivery.result"
)

// ProbeReplayEvent is one immutable source event in the replayed stream.
// Data is exactly one of Observation, Incident, or Delivery.
type ProbeReplayEvent struct {
	Seq         int64
	Digest      string // SHA-256 of the complete compact wire event.
	Kind        string
	ObservedAt  time.Time
	Observation *RegionalObservation
	Incident    *RegionalIncident
	Delivery    *RegionalDelivery
}

// ProbeReplayBatch is an ordered, contiguous batch of replayed events.
type ProbeReplayBatch struct {
	ProbeID  string
	StreamID string
	FirstSeq int64
	LastSeq  int64
	Events   []ProbeReplayEvent
	Gap      *ProbeTelemetryGap
}

// ProbeReplayRejection records a permanent rejection for a specific sequence.
type ProbeReplayRejection struct {
	Seq  int64
	Code string
}

// ProbeReplayResult is the durable outcome of ingesting a replay batch.
type ProbeReplayResult struct {
	StreamID       string
	CommittedSeq   int64
	AcceptedCount  int64
	DuplicateCount int64
	Rejected       []ProbeReplayRejection
}

// ProbeReplaySession encapsulates the authenticated session authority for replay.
type ProbeReplaySession struct {
	HubID                string
	ProbeID              string
	StreamID             string
	ConnectionGeneration int64
	OwnerID              string
}

// ProbeReplayAuthorityFacts contains only nonsecret, transaction-bound facts for
// one event. History and configuration membership are scoped to MonitorID.
type ProbeReplayAuthorityFacts struct {
	ProbeID              string
	StreamID             string
	MonitorID            int64
	MonitorExists        bool
	ConfigRevision       int64
	ConfigEffectiveAt    time.Time
	ConfigAssignment     *EdgeAssignmentIdentity
	AssignmentHistory    []AssignmentInterval
	Channels             map[int64]int64
	PriorIncident        *RegionalIncident
	ParentTransition     *RegionalIncident
	PriorDelivery        *RegionalDelivery
	DeliveryIntentExists bool
}

// ValidProbeReplayBatch verifies contiguous framing at the core boundary too.
func ValidProbeReplayBatch(session ProbeReplaySession, batch ProbeReplayBatch) bool {
	if !ValidHubID(session.HubID) || !ValidHubID(session.OwnerID) || !ValidHubID(session.ProbeID) ||
		!ValidHubID(session.StreamID) || session.ProbeID == LocalProbeID || session.StreamID == LocalStreamID ||
		session.ConnectionGeneration <= 0 || batch.ProbeID != session.ProbeID || batch.StreamID != session.StreamID ||
		batch.FirstSeq <= 0 || batch.LastSeq < batch.FirstSeq {
		return false
	}
	if batch.Gap != nil {
		g := batch.Gap
		if len(batch.Events) != 0 || g.StreamID != batch.StreamID || g.FromSeq != batch.FirstSeq || g.ThroughSeq != batch.LastSeq || g.ObservedFrom.IsZero() || g.ObservedThrough.Before(g.ObservedFrom) || len(g.AffectedMonitorIDs) > 256 {
			return false
		}
		switch g.Reason {
		case "retention_age", "retention_bytes", "disk_pressure", "restore_loss":
		default:
			return false
		}
		seen := make(map[int64]bool, len(g.AffectedMonitorIDs))
		for _, id := range g.AffectedMonitorIDs {
			if id <= 0 || seen[id] {
				return false
			}
			seen[id] = true
		}
		return true
	}
	if len(batch.Events) < 1 || len(batch.Events) > 256 || batch.LastSeq-batch.FirstSeq != int64(len(batch.Events)-1) {
		return false
	}
	for index, event := range batch.Events {
		if event.Seq != batch.FirstSeq+int64(index) || event.ObservedAt.IsZero() || !ValidKeyHash(event.Digest) {
			return false
		}
	}
	return true
}
