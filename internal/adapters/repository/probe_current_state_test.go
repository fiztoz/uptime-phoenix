package repository_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func activateReplayConfig(t *testing.T, r replayFixture) {
	t.Helper()
	metadata, err := r.syncer.RefreshRemote(t.Context(), syncTarget(), r.at)
	if err != nil {
		t.Fatal(err)
	}
	lease := domain.ProbeConnectorLease{ProbeID: r.session.ProbeID, OwnerID: r.session.OwnerID, Generation: r.session.ConnectionGeneration}
	if err := r.syncer.RecordRemoteApplied(t.Context(), lease, domain.ProbeActiveConfig{ProbeConfigTarget: syncTarget(), Revision: metadata.Revision, SHA256: metadata.SHA256, AppliedAt: r.at, AssignmentCount: 1}); err != nil {
		t.Fatal(err)
	}
}

func currentSnapshot(r replayFixture, seq int64, states ...domain.ProbeCurrentState) domain.ProbeCurrentSnapshot {
	id := uuid.NewString()
	sum := sha256.Sum256([]byte(id))
	return domain.ProbeCurrentSnapshot{ProbeID: r.session.ProbeID, StreamID: r.session.StreamID, SnapshotID: id, SHA256: hex.EncodeToString(sum[:]), ConfigRevision: 1, CreatedAt: r.at, LastCreatedSeq: seq, States: states}
}

func currentEntry(r replayFixture, seq int64, status domain.Status) domain.ProbeCurrentState {
	down := 0
	if status == domain.StatusDown {
		down = 1
	}
	return domain.ProbeCurrentState{MonitorID: r.monitor, AssignmentGeneration: 1, Seq: seq, ObservedAt: r.at, Status: status, DownCount: down, Ping: 23, Message: "source current"}
}

func TestProbeCurrentStateAcceptance(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("UpgradePreservesExactObservation", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				activateReplayConfig(t, r)
				event := r.observation(1)
				event.Observation.Ping = 23
				event.Observation.Message = "source current"
				r.ingest(t, r.batch(event))
				// Rehearse dependency order too: 064 extends a table created by
				// 056. Rebuilding only 056 would leave the current schema invalid.
				if err := runEngineMigration(t, r.f.db, engine, "064_probe_stream_reset", "down"); err != nil {
					t.Fatal(err)
				}
				if err := runEngineMigration(t, r.f.db, engine, "056_probe_current_state", "down"); err != nil {
					t.Fatal(err)
				}
				if err := runEngineMigration(t, r.f.db, engine, "056_probe_current_state", "up"); err != nil {
					t.Fatal(err)
				}
				if err := runEngineMigration(t, r.f.db, engine, "064_probe_stream_reset", "up"); err != nil {
					t.Fatal(err)
				}
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, currentSnapshot(r, 1, currentEntry(r, 1, domain.StatusUp)), &services.AccessService{}); err != nil {
					t.Fatal("upgrade discarded immutable current evidence", err)
				}
			})
			t.Run("EmptyInitialSnapshotThenFirstEvidence", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				activateReplayConfig(t, r)
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, currentSnapshot(r, 0), &services.AccessService{}); err != nil {
					t.Fatal(err)
				}
				state, err := r.f.commits.GetState(t.Context(), r.monitor, r.session.ProbeID)
				if err != nil || state.Seq != 0 || state.Status != domain.StatusUnknown || state.UnknownReason != "missing_snapshot_state" {
					t.Fatalf("empty source invented evidence: %+v %v", state, err)
				}
				r.ingest(t, r.batch(r.observation(1)))
				state, err = r.f.commits.GetState(t.Context(), r.monitor, r.session.ProbeID)
				if err != nil || state.Seq != 1 || state.Status != domain.StatusUp || state.UnknownReason != "" {
					t.Fatalf("first observation failed to clear empty-source marker: %+v %v", state, err)
				}
			})
			t.Run("SameObservationAllowsIncidentMetadataOnly", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				activateReplayConfig(t, r)
				entry := currentEntry(r, 10, domain.StatusDown)
				apply := func(s domain.ProbeCurrentSnapshot) error {
					_, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, s, &services.AccessService{})
					return err
				}
				if err := apply(currentSnapshot(r, 10, entry)); err != nil {
					t.Fatal(err)
				}
				incidentID := uuid.NewString()
				entry.ActiveSourceAlertID = &incidentID
				if err := apply(currentSnapshot(r, 11, entry)); err != nil {
					t.Fatal(err)
				}
				state, err := r.f.commits.GetState(t.Context(), r.monitor, r.session.ProbeID)
				if err != nil || state.Seq != 10 || state.ActiveSourceAlertID == nil || *state.ActiveSourceAlertID != incidentID {
					t.Fatalf("independent incident metadata lost: %+v %v", state, err)
				}
				changed := entry
				changed.Message = "changed immutable event"
				if err := apply(currentSnapshot(r, 12, changed)); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("same source sequence changed observation", err)
				}
				entry.ActiveSourceAlertID = nil
				if err := apply(currentSnapshot(r, 12, entry)); err != nil {
					t.Fatal(err)
				}
				state, err = r.f.commits.GetState(t.Context(), r.monitor, r.session.ProbeID)
				if err != nil || state.Seq != 10 || state.ActiveSourceAlertID != nil {
					t.Fatalf("resolved incident reference remained: %+v %v", state, err)
				}
				if replayCount(t, r.f, "probe_incidents") != 0 || replayCount(t, r.f, "probe_observations") != 0 {
					t.Fatal("snapshot manufactured incident or observation history")
				}
			})
			t.Run("KnownIncidentMustMatchSnapshotAssignment", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				activateReplayConfig(t, r)
				incident := r.incident(1, 1)
				r.ingest(t, r.batch(incident))
				entry := currentEntry(r, 2, domain.StatusDown)
				entry.ActiveSourceAlertID = &incident.Incident.SourceAlertID
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, currentSnapshot(r, 2, entry), &services.AccessService{}); err != nil {
					t.Fatal(err)
				}
				if _, err := r.f.db.ExecContext(t.Context(), "UPDATE probe_incidents SET assignment_generation = 2 WHERE source_alert_id = ?", incident.Incident.SourceAlertID); err != nil {
					t.Fatal(err)
				}
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, currentSnapshot(r, 3, entry), &services.AccessService{}); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("foreign incident assignment accepted", err)
				}
			})
			t.Run("AheadOfBacklogAndDurableReceipt", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				activateReplayConfig(t, r)
				candidate := currentSnapshot(r, 10, currentEntry(r, 10, domain.StatusDown))
				receipt, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, candidate, &services.AccessService{})
				if err != nil {
					t.Fatal(err)
				}
				if receipt.StateCount != 1 || receipt.SnapshotID != candidate.SnapshotID {
					t.Fatal("incorrect receipt", receipt)
				}
				if n := replayCount(t, r.f, "probe_observations"); n != 0 {
					t.Fatal("snapshot fabricated history", n)
				}
				if n := replayCount(t, r.f, "probe_delivery_intents"); n != 0 {
					t.Fatal("snapshot generated provider work", n)
				}
				if cursor, err := r.store.GetCursor(t.Context(), r.session.ProbeID, r.session.StreamID); err != nil || cursor != 0 {
					t.Fatal("snapshot advanced cursor", cursor, err)
				}
				duplicate, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, candidate, &services.AccessService{})
				if err != nil || *duplicate != *receipt {
					t.Fatalf("lost receipt retry changed identity: %+v %+v %v", receipt, duplicate, err)
				}
				r.ingest(t, r.batch(r.observation(1)))
				state, err := r.f.commits.GetState(t.Context(), r.monitor, r.session.ProbeID)
				if err != nil || state.Seq != 10 || state.Status != domain.StatusDown || state.Ping != 23 || state.Message != "source current" {
					t.Fatalf("backlog regressed live state: %+v %v", state, err)
				}
				candidate.SHA256 = hex.EncodeToString(make([]byte, 32))
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, candidate, &services.AccessService{}); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("conflicting duplicate accepted", err)
				}
				if err := runEngineMigration(t, r.f.db, engine, "056_probe_current_state", "down"); err == nil {
					t.Fatal("downgrade discarded current receipts")
				}
			})
			t.Run("OmittedAssignmentStaysUnknownUntilNewEvidence", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				activateReplayConfig(t, r)
				r.ingest(t, r.batch(r.observation(1)))
				candidate := currentSnapshot(r, 10)
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, candidate, &services.AccessService{}); err != nil {
					t.Fatal(err)
				}
				r.ingest(t, r.batch(r.observation(2)))
				states, err := r.f.commits.ListStates(t.Context(), r.monitor)
				if err != nil || len(states) != 1 || states[0].Status != domain.StatusUnknown || states[0].UnknownReason != "missing_snapshot_state" {
					t.Fatalf("old replay undid complete omission: %+v %v", states, err)
				}
				gap := domain.ProbeTelemetryGap{StreamID: r.session.StreamID, FromSeq: 3, ThroughSeq: 10, Reason: "retention_age", ObservedFrom: r.at, ObservedThrough: r.at}
				r.ingest(t, domain.ProbeReplayBatch{ProbeID: r.session.ProbeID, StreamID: r.session.StreamID, FirstSeq: 3, LastSeq: 10, Gap: &gap})
				r.ingest(t, r.batch(r.observation(11)))
				state, err := r.f.commits.GetState(t.Context(), r.monitor, r.session.ProbeID)
				if err != nil || state.Status != domain.StatusUp || state.Seq != 11 || state.UnknownReason != "" {
					t.Fatalf("new evidence failed to clear omission: %+v %v", state, err)
				}
				if n := replayCount(t, r.f, "probe_missing_state"); n != 0 {
					t.Fatal("stale barrier remained", n)
				}
			})
			t.Run("SequenceOrdersBackwardClock", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				activateReplayConfig(t, r)
				r.ingest(t, r.batch(r.observation(1)))
				later := r.observation(2)
				later.ObservedAt = r.at.Add(-time.Second)
				later.Observation.ObservedAt = later.ObservedAt
				later.Observation.Status = domain.StatusDown
				later.Observation.RawStatus = domain.StatusDown
				later.Observation.DownCount = 1
				r.ingest(t, r.batch(later))
				state, err := r.f.commits.GetState(t.Context(), r.monitor, r.session.ProbeID)
				if err != nil || state.Seq != 2 || state.Status != domain.StatusDown {
					t.Fatalf("backward clock discarded later source sequence: %+v %v", state, err)
				}
				entry := currentEntry(r, 3, domain.StatusUp)
				entry.ObservedAt = r.at.Add(-2 * time.Second)
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, currentSnapshot(r, 3, entry), &services.AccessService{}); err != nil {
					t.Fatal(err)
				}
				state, err = r.f.commits.GetState(t.Context(), r.monitor, r.session.ProbeID)
				if err != nil || state.Seq != 3 || !state.ObservedAt.Equal(entry.ObservedAt) {
					t.Fatalf("snapshot clock regression ignored: %+v %v", state, err)
				}
			})
			t.Run("AuthorityAndFinalReceiptRollback", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				activateReplayConfig(t, r)
				r.ingest(t, r.batch(r.observation(1)))
				candidate := currentSnapshot(r, 10)
				stale := r.session
				stale.ConnectionGeneration++
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), stale, candidate, &services.AccessService{}); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("stale session applied", err)
				}
				foreign := currentEntry(r, 10, domain.StatusUp)
				foreign.MonitorID = r.monitor + 100
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, currentSnapshot(r, 10, foreign), &services.AccessService{}); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("foreign state applied", err)
				}
				trigger := "CREATE TRIGGER fail_state_receipt BEFORE INSERT ON probe_state_receipts BEGIN SELECT RAISE(ABORT,'receipt fault'); END"
				if engine == "mariadb" {
					trigger = "CREATE TRIGGER fail_state_receipt BEFORE INSERT ON probe_state_receipts FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='receipt fault'"
				}
				if _, err := r.f.db.ExecContext(t.Context(), trigger); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _, _ = r.f.db.ExecContext(context.Background(), "DROP TRIGGER IF EXISTS fail_state_receipt") })
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, candidate, &services.AccessService{}); !errors.Is(err, domain.ErrInternal) {
					t.Fatal("failed receipt reported success", err)
				}
				state, err := r.f.commits.GetState(t.Context(), r.monitor, r.session.ProbeID)
				if err != nil || state.Seq != 1 || state.Status != domain.StatusUp || replayCount(t, r.f, "probe_missing_state") != 0 || replayCount(t, r.f, "probe_state_receipts") != 0 {
					t.Fatalf("partial snapshot escaped rollback: %+v %v", state, err)
				}
				if _, err := r.f.db.ExecContext(t.Context(), "DROP TRIGGER fail_state_receipt"); err != nil {
					t.Fatal(err)
				}
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, candidate, &services.AccessService{}); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}
