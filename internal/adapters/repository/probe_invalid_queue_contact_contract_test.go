package repository_test

import (
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TestInvalidQueueReportStillStampsContact pins the #171 follow-up on both
// engines. DecodeHealth accepts an oldest queued time whose UTC microsecond is
// not positive. RecordConnectorContact used to reject that whole write, and the
// connector ignores the error, so last seen froze while the session stayed up.
// An unusable report now stamps contact and leaves the stored queue alone. A
// fenced-out lease still writes nothing.
func TestInvalidQueueReportStillStampsContact(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			f.remote(t, probeRegistryID1, "invalid-queue-contact")
			ctx := t.Context()
			store := repository.NewProbeConnectorStore(f.db)
			read := func(t *testing.T) *domain.ProbeSessionFacts {
				t.Helper()
				d, err := repository.NewProbeDiagnosticsStore(f.db).GetProbeDiagnostics(ctx, probeRegistryID1)
				if err != nil {
					t.Fatal(err)
				}
				if d.Session == nil {
					t.Fatal("session facts missing")
				}
				return d.Session
			}
			rewind := func(t *testing.T) {
				t.Helper()
				if _, err := f.db.ExecContext(ctx, "UPDATE probe_sessions SET last_contact_at = ? WHERE probe_id = ?", time.Now().Add(-time.Minute).Unix(), probeRegistryID1); err != nil {
					t.Fatal(err)
				}
			}

			first, err := store.AcquireConnector(ctx, probeRegistryID1, probeRegistryID2)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetConnectorConnected(ctx, first, true); err != nil {
				t.Fatal(err)
			}

			// A first unusable report is still contact, and it does not invent a queue.
			if err := store.RecordConnectorContact(ctx, first, domain.ProbeQueueReport{QueueBytes: -1}); err != nil {
				t.Fatal(err)
			}
			session := read(t)
			if session.LastContactAt == nil || session.ReportedQueue != nil {
				t.Fatalf("first invalid report: contact %v queue %+v", session.LastContactAt, session.ReportedQueue)
			}

			rewind(t)
			oldest := time.Date(2026, 10, 28, 2, 35, 0, 123456000, time.UTC)
			if err := store.RecordConnectorContact(ctx, first, domain.ProbeQueueReport{QueueBytes: 4096, OldestQueuedAt: &oldest}); err != nil {
				t.Fatal(err)
			}
			kept := read(t).ReportedQueue
			if kept == nil || kept.QueueBytes != 4096 || kept.OldestQueuedAt == nil || !kept.OldestQueuedAt.Equal(oldest) {
				t.Fatalf("valid report not stored: %+v", kept)
			}

			epoch := time.Unix(0, 0).UTC()
			unset := time.Time{}
			unusable := []domain.ProbeQueueReport{
				{QueueBytes: -1},
				{QueueBytes: 5},
				{QueueBytes: 0, OldestQueuedAt: &oldest},
				{QueueBytes: 1, OldestQueuedAt: &epoch},
				{QueueBytes: 1, OldestQueuedAt: &unset},
			}
			for _, report := range unusable {
				rewind(t)
				before := read(t).LastContactAt
				if err := store.RecordConnectorContact(ctx, first, report); err != nil {
					t.Fatalf("report %+v: %v", report, err)
				}
				got := read(t)
				if got.LastContactAt == nil || before == nil || !got.LastContactAt.After(*before) {
					t.Fatalf("report %+v did not advance contact: %v -> %v", report, before, got.LastContactAt)
				}
				if got.ReportedQueue == nil || got.ReportedQueue.QueueBytes != kept.QueueBytes || !got.ReportedQueue.ReportedAt.Equal(kept.ReportedAt) || !equalTimePtr(got.ReportedQueue.OldestQueuedAt, kept.OldestQueuedAt) {
					t.Fatalf("report %+v changed the stored queue: %+v", report, got.ReportedQueue)
				}
			}

			foreign := domain.ProbeConnectorLease{ProbeID: probeRegistryID1, OwnerID: probeRegistryID3, Generation: first.Generation}
			before := read(t)
			if err := store.RecordConnectorContact(ctx, foreign, domain.ProbeQueueReport{QueueBytes: -1}); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("foreign owner: %v", err)
			}
			if _, err := f.db.ExecContext(ctx, "UPDATE probe_sessions SET lease_until = 1 WHERE probe_id = ?", probeRegistryID1); err != nil {
				t.Fatal(err)
			}
			if err := store.RecordConnectorContact(ctx, first, domain.ProbeQueueReport{QueueBytes: 1, OldestQueuedAt: &epoch}); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("expired lease: %v", err)
			}
			after := read(t)
			if !equalTimePtr(before.LastContactAt, after.LastContactAt) || after.ReportedQueue == nil || !after.ReportedQueue.ReportedAt.Equal(before.ReportedQueue.ReportedAt) {
				t.Fatalf("fenced report wrote contact %v -> %v or queue %+v", before.LastContactAt, after.LastContactAt, after.ReportedQueue)
			}
		})
	}
}
