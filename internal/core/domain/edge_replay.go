package domain

import "time"

// EdgeReplayItem is one raw event stored in the edge outbox.
type EdgeReplayItem struct {
	Seq        int64
	Kind       string
	ObservedAt time.Time
	Payload    []byte
}

// EdgeReplayBatch is a bounded contiguous batch read from the edge outbox.
type EdgeReplayBatch struct {
	StreamID   string
	FirstSeq   int64
	LastSeq    int64
	Items      []EdgeReplayItem
	TotalBytes int
	Gap        *ProbeTelemetryGap // Exactly one gap or Items, never both.
}

// EdgeReplayFence fences edge ACK commits to the current established session.
type EdgeReplayFence struct {
	HubID                string
	ProbeID              string
	StreamID             string
	ConnectionGeneration int64
}
