package main

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
)

// readEdgeHealth builds the health frame payload from the edge store. Storage
// writability is an actual bounded write, so a full disk reports
// db_writable=false and never advertises ready.
func readEdgeHealth(ctx context.Context, store *edge.Store, healthy bool, revision int64, stopping bool) (probe.Health, error) {
	state, err := store.ReadDiagnostics(ctx)
	if err != nil {
		return probe.Health{}, err
	}
	return edgeHealth(state, store.CheckWritable(ctx) == nil, healthy, revision, stopping), nil
}

func edgeHealth(state edge.Diagnostics, writable, healthy bool, revision int64, stopping bool) probe.Health {
	codes := []string{}
	if !writable {
		codes = append(codes, "storage_unavailable")
	}
	if !healthy {
		codes = append(codes, "scheduler_unavailable")
	}
	if state.QueuePressure {
		codes = append(codes, "queue_pressure")
	}
	if state.MetadataPressure {
		codes = append(codes, "state_pressure")
	}
	if state.GapRanges > 0 {
		codes = append(codes, "telemetry_gap")
	}
	if stopping {
		codes = append(codes, "shutting_down")
	}
	var oldest *probe.Timestamp
	if state.OldestQueuedAt != nil {
		at := probe.Timestamp(*state.OldestQueuedAt)
		oldest = &at
	}
	return probe.Health{Role: "probe", Ready: writable && healthy && revision > 0 && !stopping, DBWritable: writable, SchedulerHealthy: &healthy, ConfigRevision: probe.Decimal(revision), CommittedSeq: probe.Decimal(state.Identity.CommittedSeq), QueueBytes: &state.QueueBytes, OldestQueuedAt: oldest, ClockTime: probe.Timestamp(time.Now().UTC()), Errors: codes}
}
