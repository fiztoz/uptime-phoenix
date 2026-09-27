package domain

import "time"

// HistoryClearWatermark is the durable acknowledgement that one monitor's
// history was deliberately cleared through an explicit bound, for one probe
// assignment generation. It fences replay: evidence the source observed at or
// before ThroughObservedAt, or carried at or below ThroughSeq in ThroughStreamID,
// was removed on purpose and must never be resurrected by a replayed queue, a
// restored hub or a restored edge snapshot.
//
// The watermark is scoped by assignment generation: a recreated assignment
// (generation two) starts a fresh evidence identity and is never fenced by the
// previous generation's clear. Current state, incidents and delivery outcomes
// are not history and are never covered by a watermark.
type HistoryClearWatermark struct {
	ClearID              string
	MonitorID            int64
	ProbeID              string
	AssignmentGeneration int64
	ThroughSeq           int64
	ThroughStreamID      string
	ThroughObservedAt    time.Time
	ClearedAt            time.Time
	DroppedCount         int64
}

// Cleared reports whether one remote observation of this watermark's
// (monitor, probe, assignment generation) identity falls under the cleared
// bound.
//
// The sequence bound is scoped to ThroughStreamID: a replacement stream starts
// from sequence one without changing the assignment generation. It covers
// a hub restored behind the clear that receives replayed old sequence space.
// The observation bound covers evidence the source created before the clear
// but that had not been ingested yet. A backward source clock can therefore
// make fresh evidence look pre-clear; the drop is the conservative choice —
// resurrecting removed evidence is the worse failure — and every drop is
// counted on the watermark as an acknowledged intentional drop.
func (w HistoryClearWatermark) Cleared(streamID string, seq int64, observedAt time.Time) bool {
	// Both bounds are compared at the persisted wall-clock precision (UTC,
	// microseconds) — AGENTS.md rules 6 and 12.
	bound := w.ThroughObservedAt.UTC().Truncate(time.Microsecond)
	at := observedAt.UTC().Truncate(time.Microsecond)
	return (w.ThroughStreamID != "" && streamID == w.ThroughStreamID && seq <= w.ThroughSeq) || !at.After(bound)
}
