package repository_test

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
)

func testHistoryClearSurvivesStreamReset(t *testing.T, engine string, upgrade bool) {
	f := newStreamResetFixture(t, engine)
	ctx := t.Context()
	// Rehearse a populated legacy fence from before the reset, with explicit
	// database chronology independent of host/container clock skew.
	if _, err := f.f.db.ExecContext(ctx, "UPDATE probe_streams SET created_at = ? WHERE probe_id = ? AND stream_id = ?", f.at.Add(-time.Minute), f.session.ProbeID, f.session.StreamID); err != nil {
		t.Fatal(err)
	}
	fences, err := repository.NewHistoryClearStore(f.f.db).ClearMonitorHistory(ctx, f.monitor, f.at.Add(time.Second))
	if err != nil || len(fences) != 1 {
		t.Fatalf("clear: %v %v", fences, err)
	}
	if fences[0].ThroughStreamID != f.session.StreamID {
		t.Fatal("clear did not capture the original stream")
	}
	if upgrade {
		// Simulate 069's populated schema. The real down migration deliberately
		// refuses to discard a populated stream fence.
		if _, err := f.f.db.ExecContext(ctx, "ALTER TABLE history_clear_watermarks DROP COLUMN through_stream_id"); err != nil {
			t.Fatal(err)
		}
	}
	op := f.prepare(t)
	if _, err := f.resets.ActivateStreamReset(ctx, f.receipt(op.Plan)); err != nil {
		t.Fatal(err)
	}
	if upgrade {
		if err := runEngineMigration(t, f.f.db, engine, "070_history_clear_stream", "up"); err != nil {
			t.Fatal(err)
		}
		var stream string
		if err := f.f.db.NewRaw("SELECT through_stream_id FROM history_clear_watermarks WHERE monitor_id = ?", f.monitor).Scan(ctx, &stream); err != nil || stream != f.session.StreamID {
			t.Fatalf("legacy watermark rebound to replacement stream: %q %v", stream, err)
		}
		if err := runEngineMigration(t, f.f.db, engine, "070_history_clear_stream", "down"); err == nil {
			t.Fatal("downgrade discarded a populated stream fence")
		}
		if _, err := f.f.db.ExecContext(ctx, "DROP TABLE IF EXISTS history_clear_stream_downgrade_guard"); err != nil {
			t.Fatal(err)
		}
	}
	current, err := f.connections.GetConnection(ctx, f.session.ProbeID)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := f.connections.AcquireConnector(ctx, f.session.ProbeID, probeRegistryID3)
	if err != nil {
		t.Fatal(err)
	}
	session := f.session
	session.StreamID, session.ConnectionGeneration = f.issue.StreamID, lease.Generation
	if err := f.commands.ConfirmCredentialConnection(ctx, session, current.ProbeCredentialMetadata); err != nil {
		t.Fatal(err)
	}
	if err := f.commands.ConfirmCertificateConnection(ctx, session, current.ProbeCredentialMetadata, current.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := f.resets.ConfirmStreamReset(ctx, session, current.ProbeCredentialMetadata, current.Fingerprint); err != nil {
		t.Fatal(err)
	}
	complete, err := f.resets.GetStreamReset(ctx, f.issue.HubID, f.issue.ProbeID, f.issue.ResetID)
	if err != nil {
		t.Fatal(err)
	}
	r := f.replayFixture
	r.session, r.at = session, complete.ConfirmedAt.UTC()
	event := r.observation(1)
	if !event.Observation.ObservedAt.After(fences[0].ThroughObservedAt) {
		t.Fatal("fixture observation must be after clear")
	}
	result := r.ingest(t, r.batch(event))
	if result.AcceptedCount != 1 {
		t.Fatalf("fresh replacement-stream observation rejected by old-stream watermark: %+v, rejections=%+v", result, result.Rejected)
	}
	// Clearing again binds the replacement stream's own sequence, never the
	// retired stream's larger maximum. Timestamp protection still only widens.
	clearAt := r.at.Add(time.Second)
	fences, err = repository.NewHistoryClearStore(f.f.db).ClearMonitorHistory(ctx, f.monitor, clearAt)
	if err != nil || len(fences) != 1 || fences[0].ThroughStreamID != session.StreamID || fences[0].ThroughSeq != 1 {
		t.Fatalf("replacement fence: %+v %v", fences, err)
	}
	fences, err = repository.NewHistoryClearStore(f.f.db).ClearMonitorHistory(ctx, f.monitor, r.at)
	if err != nil || !fences[0].ThroughObservedAt.Equal(clearAt.Truncate(time.Microsecond)) || fences[0].ThroughSeq != 1 {
		t.Fatalf("clock rollback shrank fence: %+v %v", fences, err)
	}
	fresh := r.observation(2)
	fresh.ObservedAt, fresh.Observation.ObservedAt = clearAt.Add(time.Second), clearAt.Add(time.Second)
	if result := r.ingest(t, r.batch(fresh)); result.AcceptedCount != 1 {
		t.Fatalf("new stream did not continue after repeated clear: %+v", result)
	}
}

func TestHistoryClearSurvivesStreamReset(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("new_fence", func(t *testing.T) { testHistoryClearSurvivesStreamReset(t, engine, false) })
			t.Run("upgraded_fence", func(t *testing.T) { testHistoryClearSurvivesStreamReset(t, engine, true) })
		})
	}
}
