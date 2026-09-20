package edge

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestEdgeCurrentSnapshotSurvivesHistoryPruningAndRestart(t *testing.T) {
	s, dir := setupEdgeDeliveryStore(t)
	before, err := s.ReadCurrentSnapshot(t.Context(), validFence(), time.Now().UTC())
	if err != nil || len(before.States) != 0 || before.Identity.LastCreatedSeq != 0 {
		t.Fatalf("never-observed assignment fabricated state: %+v %v", before, err)
	}
	r := checkRecord()
	recorded, err := s.CommitEdgeCheck(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.ReadCurrentSnapshot(t.Context(), validFence(), time.Now().UTC())
	if err != nil || len(snapshot.States) != 1 || snapshot.States[0].Seq != recorded.Seq || snapshot.States[0].ActiveSourceAlertID == nil {
		t.Fatalf("current evidence missing: %+v %v", snapshot, err)
	}
	exact, err := probe.EdgeTelemetryEncoder{}.EncodeObservation(recorded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snapshot.States[0].Payload, exact) {
		t.Fatal("current state rebuilt source evidence")
	}
	ack := domain.ProbeReplayResult{StreamID: recorded.StreamID, CommittedSeq: snapshot.Identity.LastCreatedSeq, AcceptedCount: snapshot.Identity.LastCreatedSeq}
	if err := s.CommitReplayACK(t.Context(), validFence(), ack); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), dir, testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	after, err := s.ReadCurrentSnapshot(t.Context(), validFence(), time.Now().UTC())
	if err != nil || len(after.States) != 1 || !bytes.Equal(after.States[0].Payload, exact) || after.Identity.CommittedSeq != ack.CommittedSeq {
		t.Fatalf("pruning erased current state: %+v %v", after, err)
	}
	stale := validFence()
	stale.ConnectionGeneration++
	if _, err := s.ReadCurrentSnapshot(t.Context(), stale, time.Now().UTC()); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stale session read authorized state", err)
	}
	config := protectedConfig(t, 2)
	config.Assignments[0].Generation++
	if err := s.ActivateConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	next, err := s.ReadCurrentSnapshot(t.Context(), validFence(), time.Now().UTC())
	if err != nil || next.Identity.ConfigRevision != 2 || len(next.States) != 0 {
		t.Fatalf("old generation leaked into new snapshot: %+v %v", next, err)
	}
}

func TestEdgeCurrentSnapshotRollsBackWithSourceCounter(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	original, err := s.CommitEdgeCheck(t.Context(), checkRecord())
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.ReadCurrentSnapshot(t.Context(), validFence(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), "CREATE TRIGGER fail_current_counter BEFORE UPDATE OF last_created_seq ON edge_identity WHEN NEW.last_created_seq <> OLD.last_created_seq BEGIN SELECT RAISE(ABORT,'late counter'); END"); err != nil {
		t.Fatal(err)
	}
	r := checkRecord()
	r.ExpectedStateSeq = original.Seq
	r.ExpectedIncidentVersion = 1
	r.Incident = nil
	r.DeliveryIntents = nil
	r.Observation.Message = "must roll back"
	if _, err := s.CommitEdgeCheck(t.Context(), r); !errors.Is(err, ErrStorage) {
		t.Fatal("failed source write accepted", err)
	}
	after, err := s.ReadCurrentSnapshot(t.Context(), validFence(), time.Now().UTC())
	if err != nil || len(after.States) != 1 || !bytes.Equal(before.States[0].Payload, after.States[0].Payload) || before.Identity.LastCreatedSeq != after.Identity.LastCreatedSeq {
		t.Fatal("current evidence escaped failed source transaction", err)
	}
}
