package domain

import (
	"time"
)

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
		len(batch.Events) < 1 || len(batch.Events) > 256 || batch.FirstSeq <= 0 || batch.LastSeq < batch.FirstSeq ||
		batch.LastSeq-batch.FirstSeq != int64(len(batch.Events)-1) {
		return false
	}
	for index, event := range batch.Events {
		if event.Seq != batch.FirstSeq+int64(index) || event.ObservedAt.IsZero() || !ValidKeyHash(event.Digest) {
			return false
		}
	}
	return true
}
