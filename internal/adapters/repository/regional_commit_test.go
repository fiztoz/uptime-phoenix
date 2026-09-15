package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

const (
	localStreamID  = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	remoteStreamID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

func TestRegionalCommitKeepsIndependentRetryCounters(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			monitorID := f.monitor(t)
			if _, err := f.assignments.InitializeLocal(ctx, monitorID); err != nil {
				t.Fatal(err)
			}
			remote := f.remote(t, probeRegistryID1, "asia")
			if _, err := f.assignments.Replace(ctx, monitorID, 1, []string{"local", remote.ID}, domain.HealthPolicyAnyDown); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Second)
			local := regionalSample(monitorID, "local", localStreamID, 1, 1, domain.StatusUp, 0, now)
			if err := f.commits.Commit(ctx, domain.RegionalCommit{Observation: local, State: stateFrom(local)}); err != nil {
				t.Fatal(err)
			}
			var previous *domain.RetryState
			for seq := int64(1); seq <= 2; seq++ {
				eval := services.EvaluateRetry(previous, domain.StatusDown, 1)
				sample := regionalSample(monitorID, remote.ID, remoteStreamID, 1, seq, eval.State.Status, eval.State.DownCount, now)
				sample.RawStatus = domain.StatusDown
				if err := f.commits.Commit(ctx, domain.RegionalCommit{Observation: sample, State: stateFrom(sample)}); err != nil {
					t.Fatal(err)
				}
				previous = &eval.State
			}
			localState, err := f.commits.GetState(ctx, monitorID, "local")
			if err != nil || localState.Status != domain.StatusUp || localState.DownCount != 0 {
				t.Fatalf("local state: %+v, %v", localState, err)
			}
			asia, err := f.commits.GetState(ctx, monitorID, remote.ID)
			if err != nil || asia.Status != domain.StatusDown || asia.DownCount != 2 {
				t.Fatalf("remote state: %+v, %v", asia, err)
			}
			localRows, err := f.commits.ListObservations(ctx, monitorID, "local")
			if err != nil || len(localRows) != 1 || localRows[0].Status != domain.StatusUp {
				t.Fatalf("local history: %+v, %v", localRows, err)
			}
			remoteRows, err := f.commits.ListObservations(ctx, monitorID, remote.ID)
			if err != nil || len(remoteRows) != 2 {
				t.Fatalf("remote history: %+v, %v", remoteRows, err)
			}
			if !remoteRows[0].ObservedAt.Equal(remoteRows[1].ObservedAt) {
				t.Fatalf("same-second remote rows diverged: %v %v", remoteRows[0].ObservedAt, remoteRows[1].ObservedAt)
			}
			if remoteRows[1].ID <= remoteRows[0].ID {
				t.Fatal("same-second rows need id order")
			}
		})
	}
}

func TestProbeIngestIsIdempotentAndRejectsGaps(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			monitorID := f.monitor(t)
			if _, err := f.assignments.InitializeLocal(ctx, monitorID); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			first := regionalSample(monitorID, "local", localStreamID, 1, 1, domain.StatusUp, 0, now)
			committed, err := f.ingest.Ingest(ctx, domain.ProbeIngestBatch{
				ProbeID: "local", StreamID: localStreamID, FromSeq: 1, ThroughSeq: 1, Events: []domain.RegionalObservation{first},
			})
			if err != nil || committed != 1 {
				t.Fatalf("ingest: %d, %v", committed, err)
			}
			again, err := f.ingest.Ingest(ctx, domain.ProbeIngestBatch{
				ProbeID: "local", StreamID: localStreamID, FromSeq: 1, ThroughSeq: 1, Events: []domain.RegionalObservation{first},
			})
			if err != nil || again != 1 {
				t.Fatalf("duplicate ingest: %d, %v", again, err)
			}
			gap := regionalSample(monitorID, "local", localStreamID, 1, 3, domain.StatusUp, 0, now)
			if _, err := f.ingest.Ingest(ctx, domain.ProbeIngestBatch{
				ProbeID: "local", StreamID: localStreamID, FromSeq: 3, ThroughSeq: 3, Events: []domain.RegionalObservation{gap},
			}); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("undeclared gap: %v", err)
			}
			cursor, err := f.ingest.GetCursor(ctx, "local", localStreamID)
			if err != nil || cursor != 1 {
				t.Fatalf("cursor: %d, %v", cursor, err)
			}
		})
	}
}

func regionalSample(monitorID int64, probeID, streamID string, generation, seq int64, status domain.Status, down int, at time.Time) domain.RegionalObservation {
	return domain.RegionalObservation{
		MonitorID: monitorID, ProbeID: probeID, AssignmentGeneration: generation,
		StreamID: streamID, Seq: seq, ConfigRevision: 1,
		Status: status, RawStatus: status, DownCount: down,
		ObservedAt: at, ReceivedAt: at.Add(time.Second),
	}
}

func stateFrom(obs domain.RegionalObservation) domain.RegionalState {
	state := domain.RegionalState{
		MonitorID: obs.MonitorID, ProbeID: obs.ProbeID, AssignmentGeneration: obs.AssignmentGeneration,
		StreamID: obs.StreamID, Seq: obs.Seq, ConfigRevision: obs.ConfigRevision,
		Status: obs.Status, DownCount: obs.DownCount, ObservedAt: obs.ObservedAt, ReceivedAt: obs.ReceivedAt,
	}
	if obs.Status == domain.StatusUp {
		t := obs.ObservedAt
		state.LastSuccessAt = &t
	}
	return state
}
