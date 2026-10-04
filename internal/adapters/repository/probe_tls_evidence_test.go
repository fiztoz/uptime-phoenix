package repository_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func testTLS(at time.Time, issuer string) *domain.TLSObservation {
	return &domain.TLSObservation{NotAfter: at.Add(7*24*time.Hour + 123*time.Nanosecond), DaysRemaining: 7, Issuer: issuer}
}

func assertCurrentTLS(t *testing.T, r replayFixture, seq int64, want *domain.TLSObservation) {
	t.Helper()
	state, err := r.f.commits.GetState(t.Context(), r.monitor, r.session.ProbeID)
	if err != nil || state.Seq != seq || !domain.SameTLSObservation(state.TLS, want) {
		t.Fatalf("current TLS differs: state=%+v want=%+v err=%v", state, want, err)
	}
	_, repo := boundAuxiliary(t, r.f, r.session.ProbeID, 1)
	info, err := repo.GetByMonitorID(t.Context(), r.monitor)
	if want == nil {
		if !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("absent TLS left stale auxiliary evidence: %+v %v", info, err)
		}
	} else if err != nil || !info.NotAfter.Equal(want.NotAfter) || int64(info.DaysRemaining) != want.DaysRemaining || info.Issuer != want.Issuer || info.LastCertAlertThreshold != 0 || !info.LastCertAlertNotAfter.IsZero() {
		t.Fatalf("auxiliary TLS differs or invented alert state: %+v %v", info, err)
	}
}

func TestProbeTLSEvidenceAcceptance(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("HistoryCurrentAndNull", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				event := r.observation(1)
				event.Observation.TLS = testTLS(r.at, "first CA")
				batch := r.batch(event)
				if got := r.ingest(t, batch); got.AcceptedCount != 1 || len(got.Rejected) != 0 {
					t.Fatal("TLS event rejected", got)
				}
				assertCurrentTLS(t, r, 1, event.Observation.TLS)
				if got := r.ingest(t, batch); got.DuplicateCount != 1 || replayCount(t, r.f, "tls_info") != 1 {
					t.Fatal("lost ACK duplicated TLS", got)
				}
				next := r.observation(2)
				next.ObservedAt = r.at.Add(-time.Second)
				next.Observation.ObservedAt = next.ObservedAt
				next.Observation.TLS = testTLS(r.at, "replacement CA")
				r.ingest(t, r.batch(next))
				assertCurrentTLS(t, r, 2, next.Observation.TLS)
				r.ingest(t, r.batch(r.observation(3)))
				assertCurrentTLS(t, r, 3, nil)
				history, err := r.f.commits.ListObservations(t.Context(), r.monitor, r.session.ProbeID, r.at.Add(-time.Minute), r.at.Add(time.Minute))
				if err != nil || len(history) != 3 {
					t.Fatal("TLS history lost", err)
				}
				for _, obs := range history {
					want := map[int64]*domain.TLSObservation{1: event.Observation.TLS, 2: next.Observation.TLS, 3: nil}[obs.Seq]
					if !domain.SameTLSObservation(obs.TLS, want) {
						t.Fatalf("history changed certificate identity: %+v", obs.TLS)
					}
				}
				for _, table := range []string{"probe_incidents", "probe_delivery_intents", "alerts", "alert_escalations"} {
					if replayCount(t, r.f, table) != 0 {
						t.Fatal("TLS evidence created alert/provider work", table)
					}
				}
			})
			t.Run("SnapshotAheadOfHistoryAndImmutableTLS", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				activateReplayConfig(t, r)
				entry := currentEntry(r, 10, domain.StatusUp)
				entry.TLS = testTLS(r.at, "current CA")
				apply := func(entry domain.ProbeCurrentState) error {
					_, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, currentSnapshot(r, entry.Seq, entry), &services.AccessService{})
					return err
				}
				if err := apply(entry); err != nil {
					t.Fatal(err)
				}
				old := r.observation(1)
				old.Observation.TLS = testTLS(r.at, "old CA")
				r.ingest(t, r.batch(old))
				assertCurrentTLS(t, r, 10, entry.TLS)
				changed := entry
				changed.TLS = testTLS(r.at, "forged CA")
				if err := apply(changed); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("same source sequence changed TLS", err)
				}
				changed.TLS = nil
				if err := apply(changed); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("same source sequence removed TLS", err)
				}
				assertCurrentTLS(t, r, 10, entry.TLS)
				if err := apply(entry); err != nil {
					t.Fatal("identical TLS refresh failed", err)
				}
				if _, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, currentSnapshot(r, 10), &services.AccessService{}); err != nil {
					t.Fatal(err)
				}
				assertCurrentTLS(t, r, 10, nil)
				r.ingest(t, r.batch(r.observation(2)))
				assertCurrentTLS(t, r, 10, nil)
				entry.Seq = 11
				if err := apply(entry); err != nil {
					t.Fatal(err)
				}
				assertCurrentTLS(t, r, 11, entry.TLS)
				entry.Seq, entry.TLS = 12, nil
				if err := apply(entry); err != nil {
					t.Fatal(err)
				}
				assertCurrentTLS(t, r, 12, nil)
				if n := replayCount(t, r.f, "probe_observations"); n != 2 {
					t.Fatal("snapshot fabricated history", n)
				}
			})
			t.Run("ReplayAndSnapshotRollback", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				activateReplayConfig(t, r)
				original := r.observation(1)
				original.Observation.TLS = testTLS(r.at, "original CA")
				r.ingest(t, r.batch(original))
				for _, table := range []string{"probe_streams", "probe_state_receipts"} {
					event := "UPDATE"
					if table == "probe_state_receipts" {
						event = "INSERT"
					}
					trigger := "CREATE TRIGGER fail_tls_commit BEFORE " + event + " ON " + table + " BEGIN SELECT RAISE(ABORT, 'TLS commit fault'); END"
					if engine == "mariadb" {
						trigger = "CREATE TRIGGER fail_tls_commit BEFORE " + event + " ON " + table + " FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'TLS commit fault'"
					}
					if _, err := r.f.db.ExecContext(t.Context(), trigger); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _, _ = r.f.db.ExecContext(context.Background(), "DROP TRIGGER IF EXISTS fail_tls_commit") })
					var err error
					if table == "probe_streams" {
						next := r.observation(2)
						next.Observation.TLS = testTLS(r.at, "rolled back CA")
						_, err = r.store.IngestReplayBatch(t.Context(), r.session, r.batch(next), &services.AccessService{})
					} else {
						entry := currentEntry(r, 10, domain.StatusUp)
						entry.TLS = testTLS(r.at, "rolled back CA")
						_, err = r.store.ApplyCurrentSnapshot(t.Context(), r.session, currentSnapshot(r, 10, entry), &services.AccessService{})
					}
					if !errors.Is(err, domain.ErrInternal) {
						t.Fatal("late fault returned success or leaked storage error", err)
					}
					assertCurrentTLS(t, r, 1, original.Observation.TLS)
					if replayCount(t, r.f, "probe_observations") != 1 || replayCount(t, r.f, "probe_telemetry_receipts") != 1 || replayCount(t, r.f, "probe_state_receipts") != 0 {
						t.Fatal("TLS escaped failed transaction")
					}
					if _, err := r.f.db.ExecContext(t.Context(), "DROP TRIGGER fail_tls_commit"); err != nil {
						t.Fatal(err)
					}
				}
			})
			t.Run("StaleAuthorityAndHistoricalAssignment", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				original := r.observation(1)
				original.Observation.TLS = testTLS(r.at, "original CA")
				r.ingest(t, r.batch(original))
				next := r.observation(2)
				next.Observation.TLS = testTLS(r.at, "historical CA")
				stale := r.session
				stale.ConnectionGeneration++
				if _, err := r.store.IngestReplayBatch(t.Context(), stale, r.batch(next), &services.AccessService{}); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("stale session changed TLS", err)
				}
				if _, err := r.f.assignments.Replace(t.Context(), r.monitor, 2, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown); err != nil {
					t.Fatal(err)
				}
				if got := r.ingest(t, r.batch(next)); got.AcceptedCount != 1 {
					t.Fatal("authorized historical TLS lost", got)
				}
				assertCurrentTLS(t, r, 1, original.Observation.TLS)
			})
			t.Run("ConcurrentSnapshotAndReplay", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				activateReplayConfig(t, r)
				entry := currentEntry(r, 10, domain.StatusUp)
				entry.TLS = testTLS(r.at, "new CA")
				old := r.observation(1)
				old.Observation.TLS = testTLS(r.at, "old CA")
				batch, snapshot := r.batch(old), currentSnapshot(r, 10, entry)
				errorsCh := make(chan error, 2)
				go func() {
					_, err := r.store.IngestReplayBatch(t.Context(), r.session, batch, &services.AccessService{})
					errorsCh <- err
				}()
				go func() {
					_, err := r.store.ApplyCurrentSnapshot(t.Context(), r.session, snapshot, &services.AccessService{})
					errorsCh <- err
				}()
				for range 2 {
					if err := <-errorsCh; err != nil {
						t.Fatal(err)
					}
				}
				assertCurrentTLS(t, r, 10, entry.TLS)
			})
			t.Run("MigrationRoundTripAndEvidenceGuard", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				r.ingest(t, r.batch(r.observation(1)))
				for _, direction := range []string{"down", "up"} {
					if err := runEngineMigration(t, r.f.db, engine, "066_probe_tls_evidence", direction); err != nil {
						t.Fatal(err)
					}
				}
				assertCurrentTLS(t, r, 1, nil)
				next := r.observation(2)
				next.Observation.TLS = testTLS(r.at, "retained CA")
				r.ingest(t, r.batch(next))
				if err := runEngineMigration(t, r.f.db, engine, "066_probe_tls_evidence", "down"); err == nil {
					t.Fatal("downgrade discarded TLS evidence")
				}
				assertCurrentTLS(t, r, 2, next.Observation.TLS)
			})
			t.Run("InvalidTLSRejectedBeforePersistence", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				event := r.observation(1)
				event.Observation.TLS = testTLS(r.at, strings.Repeat("x", 257))
				if got := r.ingest(t, r.batch(event)); got.AcceptedCount != 0 || len(got.Rejected) != 1 || got.Rejected[0].Code != "event_invalid" {
					t.Fatal("unbounded TLS accepted", got)
				}
				if replayCount(t, r.f, "tls_info") != 0 || replayCount(t, r.f, "probe_observations") != 0 {
					t.Fatal("rejected TLS persisted")
				}
			})
		})
	}
}
