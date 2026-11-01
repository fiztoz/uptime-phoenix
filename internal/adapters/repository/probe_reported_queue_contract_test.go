package repository_test

import (
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestProbeReportedQueueContract pins #157 on both engines: the queue size a
// probe reports about itself in an accepted health frame is persisted with
// the hub DB clock at which the hub accepted it. It is written in the same
// fenced statement as hub-observed contact (#133), so only the live session
// generation's owner can write it; nil means never reported, zero is an empty
// queue, and the value freezes across disconnect and survives a restart.
func TestProbeReportedQueueContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			f.remote(t, probeRegistryID1, "reported-queue")
			ctx := t.Context()
			store := repository.NewProbeConnectorStore(f.db)
			read := func(t *testing.T, db *bun.DB) *domain.ProbeDiagnostics {
				t.Helper()
				d, err := repository.NewProbeDiagnosticsStore(db).GetProbeDiagnostics(ctx, probeRegistryID1)
				if err != nil {
					t.Fatal(err)
				}
				return d
			}
			reported := func(t *testing.T) *domain.ProbeReportedQueue {
				t.Helper()
				d := read(t, f.db)
				if d.Session == nil {
					t.Fatal("session facts missing")
				}
				return d.Session.ReportedQueue
			}
			rewindContact := func(t *testing.T) {
				t.Helper()
				// Contact is whole seconds and never moves backward; rewind it so
				// the next accepted frame in the same second is a newer one.
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
			if got := reported(t); got != nil {
				t.Fatalf("connect invented a reported queue: %+v", got)
			}
			if s := services.ProbeDiagnosticsSummary(time.Now(), *read(t, f.db)); s.ReportedQueue != nil {
				t.Fatalf("summary invented a reported queue: %+v", s.ReportedQueue)
			}

			// Phase: a non-empty report keeps bytes, the oldest item (microsecond
			// precision, UTC) and the hub DB clock at which it was accepted.
			oldest := time.Date(2026, 10, 28, 9, 35, 0, 123456789, time.FixedZone("ICT", 7*3600))
			if err := store.RecordConnectorContact(ctx, first, domain.ProbeQueueReport{QueueBytes: 4096, OldestQueuedAt: &oldest}); err != nil {
				t.Fatal(err)
			}
			got := reported(t)
			contact := read(t, f.db).Session.LastContactAt
			if got == nil || got.QueueBytes != 4096 || got.OldestQueuedAt == nil {
				t.Fatalf("non-empty report not stored: %+v", got)
			}
			if want := oldest.UTC().Truncate(time.Microsecond); !got.OldestQueuedAt.Equal(want) || got.OldestQueuedAt.Location() != time.UTC {
				t.Fatalf("oldest queued at %v, want %v UTC", got.OldestQueuedAt, want)
			}
			if contact == nil || !got.ReportedAt.Equal(*contact) || got.ReportedAt.Location() != time.UTC {
				t.Fatalf("reported at %v, want the accepted contact %v", got.ReportedAt, contact)
			}
			if s := services.ProbeDiagnosticsSummary(time.Now(), *read(t, f.db)); s.ReportedQueue == nil || s.ReportedQueue.QueueBytes != 4096 {
				t.Fatalf("summary dropped the reported queue: %+v", s.ReportedQueue)
			}

			// Phase: a reported zero is an empty queue, not "never reported".
			rewindContact(t)
			if err := store.RecordConnectorContact(ctx, first, domain.ProbeQueueReport{}); err != nil {
				t.Fatal(err)
			}
			got = reported(t)
			if got == nil || got.QueueBytes != 0 || got.OldestQueuedAt != nil || got.ReportedAt.IsZero() {
				t.Fatalf("empty report not stored as zero: %+v", got)
			}
			emptyAt := got.ReportedAt

			// Phase: a fenced-out session cannot rewrite it.
			foreign := domain.ProbeConnectorLease{ProbeID: probeRegistryID1, OwnerID: probeRegistryID3, Generation: first.Generation}
			if err := store.RecordConnectorContact(ctx, foreign, domain.ProbeQueueReport{QueueBytes: 1, OldestQueuedAt: &oldest}); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("foreign owner recorded a queue: %v", err)
			}
			if got := reported(t); got == nil || got.QueueBytes != 0 || !got.ReportedAt.Equal(emptyAt) {
				t.Fatalf("foreign owner changed the reported queue: %+v", got)
			}

			// Phase: disconnect and release freeze the last report.
			if err := store.SetConnectorConnected(ctx, first, false); err != nil {
				t.Fatal(err)
			}
			if err := store.ReleaseConnector(ctx, first); err != nil {
				t.Fatal(err)
			}
			if got := reported(t); got == nil || got.QueueBytes != 0 || !got.ReportedAt.Equal(emptyAt) {
				t.Fatalf("release changed the reported queue: %+v", got)
			}

			// Phase: the report survives a restart (a fresh connection pool).
			var peer *bun.DB
			if engine == "sqlite" {
				peer, err = sqlite.NewDB(f.dsn)
			} else {
				peer, err = mariadb.NewDB(f.dsn)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = peer.Close() }()
			if d := read(t, peer); d.Session == nil || d.Session.ReportedQueue == nil || !d.Session.ReportedQueue.ReportedAt.Equal(emptyAt) {
				t.Fatalf("restart lost the reported queue: %+v", d.Session)
			}
		})
	}
}
